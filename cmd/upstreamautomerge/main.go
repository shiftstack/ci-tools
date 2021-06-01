package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/cmd/generic-autobumper/bumper"
	"k8s.io/test-infra/prow/config/secret"
	"k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/labels"

	"github.com/openshift/ci-tools/pkg/promotion"
)

// TODO: update these for this bot
const (
	githubOrg    = "openshift"
	githubRepo   = "release"
	githubLogin  = "openshift-bot"
	githubTeam   = "openshift/openshift-team-developer-productivity-test-platform"
	matchTitle   = "Automate config brancher"
	remoteBranch = "auto-config-brancher"
)

type options struct {
	selfApprove bool

	githubLogin    string
	gitName        string
	gitEmail       string
	assign         string
	downstreamRepo string
	upstreamRepo   string

	promotion.FutureOptions
	flagutil.GitHubOptions
}

func parseOptions() options {
	var o options
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	o.FutureOptions.Bind(fs)
	fs.StringVar(&o.githubLogin, "github-login", githubLogin, "The GitHub username to use.")
	fs.StringVar(&o.gitName, "git-name", "", "The name to use on the git commit. Requires --git-email. If not specified, uses the system default.")
	fs.StringVar(&o.gitEmail, "git-email", "", "The email to use on the git commit. Requires --git-name. If not specified, uses the system default.")
	fs.StringVar(&o.assign, "assign", githubTeam, "The github username or group name to assign the created pull request to.")

	fs.StringVar(&o.downstreamRepo, "downstream-repo", "", "The downstream github repository that you want to merge changes into.")
	fs.StringVar(&o.upstreamRepo, "upstream-repo", "", "The upstream github repository that you want to merge changes from.")

	fs.BoolVar(&o.selfApprove, "self-approve", false, "Self-approve the PR by adding the `approved` and `lgtm` labels. Requires write permissions on the repo.")
	o.AddFlags(fs)
	o.AllowAnonymous = true
	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Errorf("cannot parse args: '%s'", os.Args[1:])
	}
	return o
}

func validateOptions(o options) error {
	if o.githubLogin == "" {
		return fmt.Errorf("--github-login cannot be empty string")
	}
	if (o.gitEmail == "") != (o.gitName == "") {
		return fmt.Errorf("--git-name and --git-email must be specified together")
	}
	if o.downstreamRepo == "" {
		return fmt.Errorf("--downstream-repo is mandatory")
	}
	if o.upstreamRepo == "" {
		return fmt.Errorf("--upstream-repo is mandatory")
	}
	if o.assign == "" {
		return fmt.Errorf("--assign is mandatory")
	}
	if err := o.FutureOptions.Validate(); err != nil {
		return err
	}
	return o.GitHubOptions.Validate(!o.Confirm)
}

func runAndCommitIfNeeded(stdout, stderr io.Writer, author, cmd string, args []string) (bool, error) {
	fullCommand := fmt.Sprintf("%s %s", filepath.Base(cmd), strings.Join(args, " "))

	logrus.Infof("Running: %s", fullCommand)
	if err := bumper.Call(stdout, stderr, cmd, args...); err != nil {
		return false, fmt.Errorf("failed to run %s: %w", fullCommand, err)
	}

	changed, err := bumper.HasChanges()
	if err != nil {
		return false, fmt.Errorf("error occurred when checking changes: %w", err)
	}

	if !changed {
		logrus.WithField("command", fullCommand).Info("No changes to commit")
		return false, nil
	}

	gitCmd := "git"
	if err := bumper.Call(stdout, stderr, gitCmd, []string{"add", "."}...); err != nil {
		return false, fmt.Errorf("failed to 'git add .': %w", err)
	}

	commitArgs := []string{"commit", "-m", fullCommand, "--author", author}
	if err := bumper.Call(stdout, stderr, gitCmd, commitArgs...); err != nil {
		return false, fmt.Errorf("fail to %s %s: %w", gitCmd, strings.Join(commitArgs, " "), err)
	}

	return true, nil
}

func main() {
	o := parseOptions()
	if err := validateOptions(o); err != nil {
		logrus.WithError(err).Fatal("Invalid arguments.")
	}

	sa := &secret.Agent{}
	if err := sa.Start([]string{o.GitHubOptions.TokenPath}); err != nil {
		logrus.WithError(err).Fatal("Failed to start secrets agent")
	}

	gc, err := o.GitHubOptions.GitHubClient(sa, !o.Confirm)
	if err != nil {
		logrus.WithError(err).Fatal("error getting GitHub client")
	}

	// set up local github env for merge
	// TODO: should this functionality be added to bumper as a function?
	stdout := bumper.HideSecretsWriter{Delegate: os.Stdout, Censor: sa}
	stderr := bumper.HideSecretsWriter{Delegate: os.Stderr, Censor: sa}
	author := fmt.Sprintf("%s <%s>", o.gitName, o.gitEmail)
	gitCmd := "git"

	err = bumper.Call(stdout, stderr, gitCmd, []string{"clone", o.downstreamRepo}...)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to 'git clone %s'", o.downstreamRepo)
	}

	home, _ := os.UserHomeDir()
	gitRepoPath := filepath.Join(home, o.downstreamRepo)
	logrus.Infof("Changing working directory to '%s' ...", gitRepoPath)
	if err := os.Chdir(gitRepoPath); err != nil {
		logrus.WithError(err).Fatal("Failed to change directory to %s", gitRepoPath)
	}

	err = bumper.Call(stdout, stderr, gitCmd, []string{"remote", "add", "upstream", o.upstreamRepo}...)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to 'git remote add upstream %s'", o.upstreamRepo)
	}

	err = bumper.Call(stdout, stderr, gitCmd, []string{"fetch", "upstream", "master"}...)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to 'git fetch upstream master'")
	}

	// TODO: is it better to just use master for the pull request so we dont have to clean up branches?
	branchName := fmt.Sprintf("upstream-merge-robot-%s", time.Now().Format(time.RFC1123))
	err = bumper.Call(stdout, stderr, gitCmd, []string{"checkout", "-b", branchName}...)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to 'git checkout -b %s'", branchName)
	}

	// merge upstream changes
	err = bumper.Call(stdout, stderr, gitCmd, []string{"merge", "upstream/" + branchName}...)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to 'git merge upstream/%s'", branchName)
	}

	// additional commands that require their own commits go here
	// after these commands run, `git add .` and `git commit -m ...` will be run automatically
	//
	// example 1: go mod vendor
	//
	// steps := []struct {
	// 		command   string
	// 		arguments []string
	// 	}{
	// 		{
	// 			command: "go mod vendor",
	// 			arguments: []string{},
	// 		},
	// 	}
	//

	steps := []struct {
		command   string
		arguments []string
	}{}

	commitsCounter := 0
	for _, step := range steps {
		committed, err := runAndCommitIfNeeded(stdout, stderr, author, step.command, step.arguments)
		if err != nil {
			logrus.WithError(err).Fatal("failed to run command and commit the changes")
		}

		if committed {
			commitsCounter++
		}
	}
	if commitsCounter == 0 {
		logrus.Info("no new commits, existing ...")
		os.Exit(0)
	}

	title := fmt.Sprintf("%s by auto-config-brancher job at %s", matchTitle, time.Now().Format(time.RFC1123))
	if err := bumper.GitPush(fmt.Sprintf("https://%s:%s@github.com/%s/%s.git", o.githubLogin, string(sa.GetTokenGenerator(o.GitHubOptions.TokenPath)()), o.githubLogin, githubRepo), remoteBranch, stdout, stderr, ""); err != nil {
		logrus.WithError(err).Fatal("Failed to push changes.")
	}

	var labelsToAdd []string
	if o.selfApprove {
		logrus.Infof("Self-approving PR by adding the %q and %q labels", labels.Approved, labels.LGTM)
		labelsToAdd = append(labelsToAdd, labels.Approved, labels.LGTM)
	}
	if err := bumper.UpdatePullRequestWithLabels(gc, githubOrg, githubRepo, title, fmt.Sprintf("/cc @%s", o.assign), o.githubLogin+":"+remoteBranch, "master", remoteBranch, true, labelsToAdd, false); err != nil {
		logrus.WithError(err).Fatal("PR creation failed.")
	}
}
