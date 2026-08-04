package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/prompt"
)

// FakeRuntime is a deterministic stand-in for a real coding agent.
//
// It never calls an LLM and never needs credentials, but it goes through
// exactly the same motions as a real runtime: it reads the prompt file, writes
// files inside the worktree, and reports a structured result. That is what
// lets you exercise the entire pipeline end to end before wiring up a real
// agent.
type FakeRuntime struct{}

// Name implements Runtime.
func (FakeRuntime) Name() string { return Fake }

// Run implements Runtime.
func (f FakeRuntime) Run(rc RunContext) (Result, error) {
	switch rc.Task.Kind {
	case api.KindRefineTicket:
		return f.refine(rc)
	default:
		return f.implement(rc)
	}
}

// refine produces a filled-in ticket from the issue text.
func (f FakeRuntime) refine(rc RunContext) (Result, error) {
	issueText, ok := prompt.ExtractUntrusted(rc.Prompt)
	if !ok {
		issueText = rc.Prompt
	}

	title, body := splitTitleBody(issueText)
	ambiguous, reason := prompt.LooksAmbiguous(title, body)

	rc.Log("fake runtime: refining %q", title)
	rc.Log("fake runtime: ambiguity check -> needs_human=%v %s", ambiguous, reason)

	ticket := buildRefinedTicket(rc.Task, title, body, ambiguous, reason)

	// Write the ticket where the worker (and a real agent) expects it.
	out := filepath.Join(rc.WorktreeDir, ".factory-refined.md")
	if err := os.WriteFile(out, []byte(ticket), 0o644); err != nil {
		return Result{}, fmt.Errorf("write refined ticket: %w", err)
	}
	rc.Log("fake runtime: wrote .factory-refined.md (%d bytes)", len(ticket))

	summary := "refined into a structured ticket"
	if ambiguous {
		summary = "could not refine confidently: " + reason
	}
	return Result{
		Summary:       summary,
		RefinedTicket: ticket,
		NeedsHuman:    ambiguous,
		Reason:        reason,
	}, nil
}

// implement makes a small, visible change so the worktree really does differ
// from the base branch.
func (f FakeRuntime) implement(rc RunContext) (Result, error) {
	dir := filepath.Join(rc.WorktreeDir, "factory-output")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create factory-output: %w", err)
	}

	path := filepath.Join(dir, rc.Task.ID+".md")
	content := buildImplementNote(rc.Task)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", path, err)
	}

	rel := filepath.ToSlash(filepath.Join("factory-output", rc.Task.ID+".md"))
	rc.Log("fake runtime: wrote %s", rel)
	rc.Log("fake runtime: a real agent would have edited source files here")

	return Result{Summary: "fake runtime created " + rel}, nil
}

// splitTitleBody separates the "Title: ..." first line the prompt builder adds.
func splitTitleBody(text string) (title, body string) {
	trimmed := strings.TrimSpace(text)
	first, rest, ok := strings.Cut(trimmed, "\n")
	if ok && strings.HasPrefix(first, "Title: ") {
		return strings.TrimPrefix(first, "Title: "), strings.TrimSpace(rest)
	}
	return strings.TrimSpace(first), strings.TrimSpace(rest)
}

// buildRefinedTicket fills the required template. It is deliberately
// mechanical: every line is derived from the issue, nothing is invented.
func buildRefinedTicket(task api.Task, title, body string, ambiguous bool, reason string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Goal\n\n%s\n\n", firstSentence(title))

	b.WriteString("## Background\n\n")
	if body == "" {
		b.WriteString("The issue body was empty, so there is no additional context.\n\n")
	} else {
		fmt.Fprintf(&b, "Reported on %s:\n\n%s\n\n", task.FullName(), quote(truncate(body, 800)))
	}

	b.WriteString("## Scope\n\n")
	fmt.Fprintf(&b, "- Address the behaviour described in %s\n", issueRef(task))
	b.WriteString("- Keep the change as small as it can be while satisfying the acceptance criteria\n\n")

	b.WriteString("## Out of Scope\n\n")
	b.WriteString("- Unrelated refactors\n- Dependency upgrades\n- Anything not required by the acceptance criteria\n\n")

	b.WriteString("## Acceptance Criteria\n")
	for _, c := range acceptanceCriteria(title, body) {
		fmt.Fprintf(&b, "- [ ] %s\n", c)
	}
	b.WriteString("\n")

	b.WriteString("## Test Plan\n\n")
	b.WriteString("- Add or update an automated test that fails before the change and passes after it\n")
	b.WriteString("- Run the project's full test suite\n\n")

	b.WriteString("## Risk Notes\n\n")
	if ambiguous {
		fmt.Fprintf(&b, "BLOCKED: %s.\nA human needs to clarify this before an agent should touch the code.\n\n", reason)
	} else {
		b.WriteString("Low: the change is scoped to the behaviour described above and is covered by tests.\n\n")
	}

	b.WriteString("## Suggested Files / Areas\n\n")
	b.WriteString("- To be identified by the implementing agent from the repository layout\n\n")

	b.WriteString("## Agent Instructions\n\n")
	if ambiguous {
		b.WriteString("Do not implement this yet. Wait for a human to clarify the request.\n")
	} else {
		b.WriteString("1. Reproduce the current behaviour\n")
		b.WriteString("2. Make the smallest change that satisfies the acceptance criteria\n")
		b.WriteString("3. Add tests, then run the full suite\n")
		b.WriteString("4. Leave the branch ready for human review; do not merge\n")
	}

	fmt.Fprintf(&b, "\n---\nGenerated by the `fake` runtime for task `%s` at %s.\n",
		task.ID, time.Now().UTC().Format(time.RFC3339))
	return b.String()
}

// acceptanceCriteria derives checkboxes from the issue text.
func acceptanceCriteria(title, body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		lower := strings.ToLower(l)
		if strings.HasPrefix(lower, "should ") || strings.Contains(lower, " should ") ||
			strings.HasPrefix(lower, "expected") || strings.HasPrefix(lower, "when i ") {
			out = append(out, truncate(strings.TrimLeft(l, "-* "), 160))
		}
		if len(out) >= 4 {
			break
		}
	}
	if len(out) == 0 {
		out = append(out, fmt.Sprintf("%s is implemented and verified", firstSentence(title)))
		out = append(out, "The change is covered by at least one automated test")
	}
	return out
}

func buildImplementNote(task api.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Fake implementation for task %s\n\n", task.ID)
	fmt.Fprintf(&b, "- Repository: %s\n", task.FullName())
	fmt.Fprintf(&b, "- Kind: %s\n", task.Kind)
	if task.GitHubIssueNumber != nil {
		fmt.Fprintf(&b, "- Issue: #%d\n", *task.GitHubIssueNumber)
	}
	fmt.Fprintf(&b, "- Title: %s\n", task.Title)
	fmt.Fprintf(&b, "- Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("This file exists so the worktree has a real, reviewable change.\n")
	b.WriteString("A real runtime (codex, claude, or your own) would edit source files instead.\n")
	return b.String()
}

// ---------- small text helpers ----------

func issueRef(task api.Task) string {
	if task.GitHubIssueNumber != nil {
		return fmt.Sprintf("%s#%d", task.FullName(), *task.GitHubIssueNumber)
	}
	return task.FullName()
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Clarify and implement the request"
	}
	if i := strings.IndexAny(s, ".\n"); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func quote(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}
