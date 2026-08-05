package worker

import (
	"fmt"
	"strings"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/githubcli"
	"github.com/HoaViet-Tech/factory/internal/gitx"
	"github.com/HoaViet-Tech/factory/internal/labels"
	"github.com/HoaViet-Tech/factory/internal/prompt"
)

// requiredLabel is the label an issue must still carry for a given task kind.
func requiredLabel(kind string) string {
	switch kind {
	case api.KindRefineTicket:
		return labels.Inbox
	case api.KindImplementTicket:
		return labels.Ready
	case api.KindReviewPR:
		return labels.Review
	default:
		return ""
	}
}

// revalidateIssue fetches the issue's live state and checks that the label
// which triggered this task is still present.
//
// This is the guard against acting on a stale snapshot: between polling and
// execution a human may have removed the label, closed the issue, or already
// handled it. When that happens the task finishes successfully having done
// nothing, and the returned skip message explains why.
func (w *Worker) revalidateIssue(task api.Task, logf func(string, string, ...any)) (*githubcli.Issue, string, error) {
	repo := task.FullName()
	number := *task.GitHubIssueNumber

	logf(api.EventGitHub, "fetching live state of %s#%d before touching anything", repo, number)
	iss, err := w.cfg.GitHub.ViewIssue(repo, number)
	if err != nil {
		return nil, "", fmt.Errorf("fetch issue %s#%d: %w", repo, number, err)
	}

	if strings.EqualFold(iss.State, "closed") {
		msg := fmt.Sprintf("issue %s#%d is closed; skipping", repo, number)
		logf(api.EventGitHub, "%s", msg)
		return nil, msg, nil
	}

	want := requiredLabel(task.Kind)
	if want != "" && !iss.HasLabel(want) {
		msg := fmt.Sprintf("issue %s#%d no longer has %s (labels now: %s); skipping",
			repo, number, want, strings.Join(iss.Labels, ", "))
		logf(api.EventGitHub, "%s", msg)
		return nil, msg, nil
	}

	// Mark the issue as in-flight so a human watching GitHub can see it.
	inFlight := map[string]string{
		api.KindRefineTicket:    labels.Refining,
		api.KindImplementTicket: labels.Active,
	}[task.Kind]
	if inFlight != "" {
		if err := w.cfg.GitHub.SetLabels(repo, number, []string{inFlight}, nil); err != nil {
			// Not fatal: a missing label should not stop real work.
			logf(api.EventWarn, "could not add %s label: %v", inFlight, err)
		}
	}
	return &iss, "", nil
}

// publishRefinement comments the structured ticket back on the issue and moves
// the labels to either ready or needs-human.
func (w *Worker) publishRefinement(task api.Task, iss githubcli.Issue, result runtimeResult, logf func(string, string, ...any)) error {
	repo := task.FullName()
	number := iss.Number

	body := result.RefinedTicket
	if strings.TrimSpace(body) == "" {
		body = "_The runtime produced no refined ticket._"
	}

	verdict := labels.Ready
	header := "🏭 **Refined ticket** — this issue is ready to implement."
	if result.NeedsHuman {
		verdict = labels.NeedsHuman
		header = "🏭 **Needs a human** — this issue is too ambiguous to implement safely."
	}

	comment := fmt.Sprintf("%s\n\n%s\n\n---\n<sub>factory task `%s` · runtime `%s`%s</sub>\n",
		header, body, task.ID, w.cfg.Runtime.Name(), reasonSuffix(result.Reason))

	if err := w.cfg.GitHub.Comment(repo, number, comment); err != nil {
		return fmt.Errorf("comment refined ticket: %w", err)
	}
	logf(api.EventGitHub, "commented refined ticket on %s#%d", repo, number)

	// Remove the trigger label so the issue cannot be picked up again, and add
	// the verdict label that decides what happens next.
	if err := w.cfg.GitHub.SetLabels(repo, number,
		[]string{verdict},
		[]string{labels.Inbox, labels.Refining},
	); err != nil {
		return fmt.Errorf("update labels: %w", err)
	}
	logf(api.EventGitHub, "labels on %s#%d: +%s -%s -%s", repo, number, verdict, labels.Inbox, labels.Refining)
	return nil
}

// publishImplementation commits the agent's work and, when pushing is enabled,
// opens a draft PR. It never merges and never deletes a branch.
func (w *Worker) publishImplementation(task api.Task, iss githubcli.Issue, wt *gitx.Worktree, baseRef string, result runtimeResult, logf func(string, string, ...any)) error {
	repo := task.FullName()
	number := iss.Number

	changed, err := wt.HasChanges()
	if err != nil {
		return fmt.Errorf("check worktree status: %w", err)
	}

	if !changed {
		msg := fmt.Sprintf("🏭 The agent finished without changing any files.\n\n%s\n\n---\n<sub>factory task `%s`</sub>\n",
			orDefault(result.Summary, "No summary was produced."), task.ID)
		if err := w.cfg.GitHub.Comment(repo, number, msg); err != nil {
			return fmt.Errorf("comment no-change summary: %w", err)
		}
		if err := w.cfg.GitHub.SetLabels(repo, number, []string{labels.Blocked}, []string{labels.Active}); err != nil {
			return fmt.Errorf("update labels: %w", err)
		}
		logf(api.EventGitHub, "no changes produced; marked %s#%d as %s", repo, number, labels.Blocked)
		return nil
	}

	commitMsg := fmt.Sprintf("%s\n\nRefs #%d\nFactory task: %s\n", iss.Title, number, task.ID)
	if err := wt.CommitAll(commitMsg, "code-factory", "code-factory@example.invalid"); err != nil {
		return fmt.Errorf("commit changes: %w", err)
	}
	logf(api.EventInfo, "committed changes on %s", wt.Branch)

	if stat, err := wt.Diffstat(baseRef); err == nil && stat != "" {
		logf(api.EventInfo, "diffstat:\n%s", stat)
	}

	if !w.cfg.Push {
		msg := fmt.Sprintf("branch %s committed locally; pushing is disabled (start the worker with --push to open a draft PR)", wt.Branch)
		logf(api.EventInfo, "%s", msg)
		if err := w.cfg.GitHub.Comment(repo, number,
			fmt.Sprintf("🏭 The agent produced changes on local branch `%s`, but this worker runs with pushing disabled.\n\n%s\n\n---\n<sub>factory task `%s`</sub>\n",
				wt.Branch, orDefault(result.Summary, ""), task.ID)); err != nil {
			return fmt.Errorf("comment local-only summary: %w", err)
		}
		return w.cfg.GitHub.SetLabels(repo, number, []string{labels.Review}, []string{labels.Active})
	}

	if err := wt.Push(); err != nil {
		return fmt.Errorf("push branch: %w", err)
	}
	logf(api.EventGitHub, "pushed branch %s", wt.Branch)

	base, err := w.cfg.GitHub.DefaultBranch(repo)
	if err != nil {
		logf(api.EventWarn, "could not read default branch, falling back to main: %v", err)
		base = "main"
	}

	prBody := fmt.Sprintf(
		"Automated draft PR from the code factory.\n\nCloses #%d\n\n## What the agent did\n\n%s\n\n"+
			"## Review notes\n\n- This PR is a **draft** and will never be merged automatically.\n"+
			"- The agent worked in an isolated worktree on branch `%s`.\n\n---\n<sub>factory task `%s` · runtime `%s`</sub>\n",
		number, orDefault(result.Summary, "See the commit for details."), wt.Branch, task.ID, w.cfg.Runtime.Name())

	url, err := w.cfg.GitHub.CreateDraftPR(repo, wt.Branch, base, "["+task.ID+"] "+iss.Title, prBody)
	if err != nil {
		return fmt.Errorf("create draft PR: %w", err)
	}
	if url != "" {
		logf(api.EventGitHub, "opened draft PR %s", url)
	}

	comment := fmt.Sprintf("🏭 Draft PR opened: %s\n\n%s\n\n---\n<sub>factory task `%s`</sub>\n",
		orDefault(url, "(dry run: no PR created)"), orDefault(result.Summary, ""), task.ID)
	if err := w.cfg.GitHub.Comment(repo, number, comment); err != nil {
		return fmt.Errorf("comment PR link: %w", err)
	}
	return w.cfg.GitHub.SetLabels(repo, number, []string{labels.Review}, []string{labels.Active})
}

// resolvePRForReview finds the open PR a review task should look at.
//
// It returns a skip message rather than an error when there is simply nothing
// to review yet: that is a normal state, not a failure.
func (w *Worker) resolvePRForReview(task api.Task, issue *githubcli.Issue, logf func(string, string, ...any)) (*githubcli.PullRequest, string, error) {
	if issue == nil || w.cfg.GitHub == nil {
		return nil, "", fmt.Errorf("review tasks need GitHub access; run the worker with an authenticated gh")
	}

	repo := task.FullName()
	pr, err := w.cfg.GitHub.FindPRForIssue(repo, issue.Number)
	if err != nil {
		return nil, "", fmt.Errorf("find PR for issue #%d: %w", issue.Number, err)
	}
	if pr == nil {
		msg := fmt.Sprintf("no open pull request references %s#%d yet; nothing to review", repo, issue.Number)
		logf(api.EventGitHub, "%s", msg)
		return nil, msg, nil
	}

	logf(api.EventGitHub, "reviewing PR #%d (%s -> %s): %s",
		pr.Number, pr.HeadRefName, pr.BaseRefName, pr.URL)
	return pr, "", nil
}

// buildReviewPrompt rebuilds the review prompt with the PR that was actually
// found, replacing the placeholder the poller created.
func buildReviewPrompt(task api.Task, issue *githubcli.Issue, pr *githubcli.PullRequest) string {
	return prompt.ForReview(
		prompt.IssueContext{
			Repo:   task.FullName(),
			Number: issue.Number,
			Title:  issue.Title,
			Body:   issue.Body,
			Author: issue.Author,
			URL:    issue.URL,
		},
		prompt.PRContext{
			Number:      pr.Number,
			Title:       pr.Title,
			Body:        pr.Body,
			URL:         pr.URL,
			HeadRefName: pr.HeadRefName,
			BaseRefName: pr.BaseRefName,
		},
	)
}

// publishReview posts the review on the pull request and moves the issue's
// label according to the verdict.
//
// The review is posted as a plain comment, never as a GitHub review approval:
// an agent must not be able to satisfy a human approval requirement on a
// protected branch.
func (w *Worker) publishReview(task api.Task, iss githubcli.Issue, pr githubcli.PullRequest, result runtimeResult, logf func(string, string, ...any)) error {
	repo := task.FullName()

	body := result.Review
	if strings.TrimSpace(body) == "" {
		body = "_The runtime produced no review document._"
	}

	verdict := result.Verdict
	if verdict == "" {
		verdict = prompt.ParseVerdict(body)
	}

	header := map[string]string{
		prompt.VerdictApprove:        "🏭 **Automated review: no blocking issues found.** A human still needs to approve and merge.",
		prompt.VerdictRequestChanges: "🏭 **Automated review: changes requested.**",
		prompt.VerdictComment:        "🏭 **Automated review.**",
	}[verdict]

	comment := fmt.Sprintf("%s\n\n%s\n\n---\n<sub>factory task `%s` · runtime `%s` · verdict `%s`</sub>\n",
		header, body, task.ID, w.cfg.Runtime.Name(), verdict)

	if err := w.cfg.GitHub.CommentOnPR(repo, pr.Number, comment); err != nil {
		return fmt.Errorf("comment review on PR #%d: %w", pr.Number, err)
	}
	logf(api.EventGitHub, "posted review on PR #%d (verdict %s)", pr.Number, verdict)

	// Only a REQUEST_CHANGES moves the issue, and it moves it to blocked so a
	// human notices. A clean review deliberately leaves the issue in
	// factory:review, because merging is a human decision either way.
	if verdict == prompt.VerdictRequestChanges {
		if err := w.cfg.GitHub.SetLabels(repo, iss.Number, []string{labels.Blocked}, []string{labels.Review}); err != nil {
			return fmt.Errorf("update labels: %w", err)
		}
		logf(api.EventGitHub, "labels on %s#%d: +%s -%s", repo, iss.Number, labels.Blocked, labels.Review)
	}
	return nil
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " · " + reason
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
