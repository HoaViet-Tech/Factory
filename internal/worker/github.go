package worker

import (
	"fmt"
	"strings"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/githubcli"
	"github.com/HoaViet-Tech/factory/internal/gitx"
	"github.com/HoaViet-Tech/factory/internal/labels"
)

// requiredLabel is the label an issue must still carry for a given task kind.
func requiredLabel(kind string) string {
	switch kind {
	case api.KindRefineTicket:
		return labels.Inbox
	case api.KindImplementTicket:
		return labels.Ready
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
