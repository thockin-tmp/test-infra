/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package trigger

import (
	"fmt"
	"regexp"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
	"k8s.io/test-infra/prow/plugins"
)

var okToTest = regexp.MustCompile(`(?m)^/ok-to-test\s*$`)
var retest = regexp.MustCompile(`(?m)^/retest\s*$`)

func handleIC(c client, trustedOrg string, ic github.IssueCommentEvent) error {
	org := ic.Repo.Owner.Login
	repo := ic.Repo.Name
	number := ic.Issue.Number
	commentAuthor := ic.Comment.User.Login
	// Only take action when a comment is first created.
	if ic.Action != github.IssueCommentActionCreated {
		return nil
	}
	// If it's not an open PR, skip it.
	if !ic.Issue.IsPullRequest() {
		return nil
	}
	if ic.Issue.State != "open" {
		return nil
	}
	// Skip bot comments.
	botName, err := c.GitHubClient.BotName()
	if err != nil {
		return err
	}
	if commentAuthor == botName {
		return nil
	}

	var changedFiles []string
	files := func() ([]string, error) {
		if changedFiles != nil {
			return changedFiles, nil
		}
		changes, err := c.GitHubClient.GetPullRequestChanges(org, repo, number)
		if err != nil {
			return nil, err
		}
		changedFiles = []string{}
		for _, change := range changes {
			changedFiles = append(changedFiles, change.Filename)
		}
		return changedFiles, nil
	}

	// Which jobs does the comment want us to run?
	testAll := okToTest.MatchString(ic.Comment.Body)
	shouldRetestFailed := retest.MatchString(ic.Comment.Body)
	requestedJobs, err := c.Config.MatchingPresubmits(ic.Repo.FullName, ic.Comment.Body, testAll, files)
	if err != nil {
		return err
	}
	if !shouldRetestFailed && len(requestedJobs) == 0 {
		// Check for the presence of the needs-ok-to-test label and remove it
		// if a trusted member has commented "/ok-to-test".
		if testAll && ic.Issue.HasLabel(needsOkToTest) {
			orgMember, err := isUserTrusted(c.GitHubClient, commentAuthor, trustedOrg, org)
			if err != nil {
				return err
			}
			if orgMember {
				return c.GitHubClient.RemoveLabel(ic.Repo.Owner.Login, ic.Repo.Name, ic.Issue.Number, needsOkToTest)
			}
		}
		return nil
	}

	pr, err := c.GitHubClient.GetPullRequest(org, repo, number)
	if err != nil {
		return err
	}

	if shouldRetestFailed {
		combinedStatus, err := c.GitHubClient.GetCombinedStatus(org, repo, pr.Head.SHA)
		if err != nil {
			return err
		}
		skipContexts := make(map[string]bool) // these succeeded or are running
		runContexts := make(map[string]bool)  // these failed and should be re-run
		for _, status := range combinedStatus.Statuses {
			state := status.State
			if state == github.StatusSuccess || state == github.StatusPending {
				skipContexts[status.Context] = true
			} else if state == github.StatusError || state == github.StatusFailure {
				runContexts[status.Context] = true
			}
		}
		retests, err := c.Config.RetestPresubmits(ic.Repo.FullName, skipContexts, runContexts, files)
		if err != nil {
			return err
		}
		requestedJobs = append(requestedJobs, retests...)
	}

	var comments []github.IssueComment
	// Skip untrusted users.
	orgMember, err := isUserTrusted(c.GitHubClient, commentAuthor, trustedOrg, org)
	if err != nil {
		return err
	}
	if !orgMember {
		comments, err = c.GitHubClient.ListIssueComments(org, repo, number)
		if err != nil {
			return err
		}
		trusted, err := trustedPullRequest(c.GitHubClient, *pr, trustedOrg, comments)
		if err != nil {
			return err
		}
		if !trusted {
			var more string
			if org != trustedOrg {
				more = fmt.Sprintf("or [%s](https://github.com/orgs/%s/people) ", org, org)
			}
			resp := fmt.Sprintf("you can't request testing unless you are a [%s](https://github.com/orgs/%s/people) %smember.", trustedOrg, trustedOrg, more)
			c.Logger.Infof("Commenting \"%s\".", resp)
			return c.GitHubClient.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, resp))
		}
	}

	if testAll && ic.Issue.HasLabel(needsOkToTest) {
		if err := c.GitHubClient.RemoveLabel(ic.Repo.Owner.Login, ic.Repo.Name, ic.Issue.Number, needsOkToTest); err != nil {
			c.Logger.WithError(err).Errorf("Failed at removing %s label", needsOkToTest)
		}
		err = clearStaleComments(c.GitHubClient, trustedOrg, *pr, comments)
		if err != nil {
			c.Logger.Warnf("Failed to clear stale comments: %v.", err)
		}
	}

	ref, err := c.GitHubClient.GetRef(org, repo, "heads/"+pr.Base.Ref)
	if err != nil {
		return err
	}

	// Determine if any branch-shard of a given job runs against the base branch.
	anyShardRunsAgainstBranch := map[string]bool{}
	for _, job := range requestedJobs {
		if job.RunsAgainstBranch(pr.Base.Ref) {
			anyShardRunsAgainstBranch[job.Context] = true
		}
	}

	var errors []error
	for _, job := range requestedJobs {
		if !job.RunsAgainstBranch(pr.Base.Ref) {
			if !job.SkipReport && !anyShardRunsAgainstBranch[job.Context] {
				if err := c.GitHubClient.CreateStatus(org, repo, pr.Head.SHA, github.Status{
					State:       github.StatusSuccess,
					Context:     job.Context,
					Description: "Skipped",
				}); err != nil {
					return err
				}
			}
			continue
		}

		c.Logger.Infof("Starting %s build.", job.Name)
		kr := kube.Refs{
			Org:     org,
			Repo:    repo,
			BaseRef: pr.Base.Ref,
			BaseSHA: ref,
			Pulls: []kube.Pull{
				{
					Number: number,
					Author: pr.User.Login,
					SHA:    pr.Head.SHA,
				},
			},
		}
		labels := make(map[string]string)
		for k, v := range job.Labels {
			labels[k] = v
		}
		labels[github.EventGUID] = ic.GUID
		pj := pjutil.NewProwJob(pjutil.PresubmitSpec(job, kr), labels)
		c.Logger.WithFields(pjutil.ProwJobFields(&pj)).Info("Creating a new prowjob.")
		if _, err := c.KubeClient.CreateProwJob(pj); err != nil {
			errors = append(errors, err)
		}
	}
	if len(errors) > 0 {
		return fmt.Errorf("errors starting jobs: %v", errors)
	}
	return nil
}
