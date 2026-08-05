package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/prompt"
)

func newRunContext(t *testing.T, task api.Task, promptText string) RunContext {
	t.Helper()
	dir := t.TempDir()
	promptFile := filepath.Join(dir, ".factory-task.md")
	if err := os.WriteFile(promptFile, []byte(promptText), 0o644); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}
	return RunContext{
		Ctx:         context.Background(),
		Task:        task,
		WorktreeDir: dir,
		PromptFile:  promptFile,
		Prompt:      promptText,
		Log:         func(format string, args ...any) { t.Logf(format, args...) },
	}
}

func TestFakeRuntimeRefineFillsEveryHeading(t *testing.T) {
	issue := 5
	task := api.Task{
		ID: "abc123", Kind: api.KindRefineTicket,
		RepoOwner: "local", RepoName: "demo", GitHubIssueNumber: &issue,
		Title: "Refine #5",
	}
	promptText := prompt.ForRefine(prompt.IssueContext{
		Repo: "local/demo", Number: 5, Author: "someone",
		Title: "Login button does nothing",
		Body:  "When I click login on mobile nothing happens. It should open the login form.",
	})

	rc := newRunContext(t, task, promptText)
	res, err := FakeRuntime{}.Run(rc)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.NeedsHuman {
		t.Errorf("a concrete issue should not need a human: %s", res.Reason)
	}
	for _, heading := range prompt.RefinedTicketHeadings() {
		if !strings.Contains(res.RefinedTicket, heading) {
			t.Errorf("refined ticket is missing the %q heading", heading)
		}
	}

	// The ticket must also be on disk, which is where the worker reads it from.
	data, err := os.ReadFile(filepath.Join(rc.WorktreeDir, ".factory-refined.md"))
	if err != nil {
		t.Fatalf("read .factory-refined.md: %v", err)
	}
	if string(data) != res.RefinedTicket {
		t.Error("the file and the returned ticket should match")
	}
}

func TestFakeRuntimeRefineFlagsVagueIssues(t *testing.T) {
	task := api.Task{ID: "vague1", Kind: api.KindRefineTicket, RepoOwner: "local", RepoName: "demo"}
	promptText := prompt.ForRefine(prompt.IssueContext{
		Repo: "local/demo", Number: 6, Author: "someone",
		Title: "make it better", Body: "idk",
	})

	res, err := FakeRuntime{}.Run(newRunContext(t, task, promptText))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.NeedsHuman {
		t.Fatal("a two-word issue should be flagged as needing a human")
	}
	if res.Reason == "" {
		t.Error("a needs-human verdict should explain itself")
	}
	if !strings.Contains(res.RefinedTicket, "BLOCKED") {
		t.Error("the ticket's Risk Notes should be marked BLOCKED")
	}
}

func TestFakeRuntimeImplementWritesAFile(t *testing.T) {
	task := api.Task{ID: "impl99", Kind: api.KindImplementTicket, RepoOwner: "local", RepoName: "demo", Title: "Add a thing"}

	rc := newRunContext(t, task, "implement the thing")
	res, err := FakeRuntime{}.Run(rc)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	out := filepath.Join(rc.WorktreeDir, "factory-output", "impl99.md")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("expected the runtime to create %s: %v", out, err)
	}
	if !strings.Contains(string(data), "impl99") {
		t.Error("the generated file should mention the task ID")
	}
	if !strings.Contains(res.Summary, "factory-output/impl99.md") {
		t.Errorf("summary = %q, want it to name the file it wrote", res.Summary)
	}
}

func TestFakeRuntimeReviewFindsProblemsInADiff(t *testing.T) {
	task := api.Task{ID: "rev001", Kind: api.KindReviewPR, RepoOwner: "local", RepoName: "demo"}

	diff := `diff --git a/auth.go b/auth.go
--- a/auth.go
+++ b/auth.go
@@ -1,3 +1,6 @@
 package auth
+
+// TODO: handle expiry
+func Login(u string) { panic("not done") }
`
	promptText := prompt.WithDiff(
		prompt.ForReview(
			prompt.IssueContext{Repo: "local/demo", Number: 4, Title: "Add login", Body: "It should log users in."},
			prompt.PRContext{Number: 11, Title: "Add login", HeadRefName: "factory/task-x-attempt-1", BaseRefName: "main"},
		), diff)

	rc := newRunContext(t, task, promptText)
	res, err := FakeRuntime{}.Run(rc)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Verdict != prompt.VerdictRequestChanges {
		t.Errorf("verdict = %q, want REQUEST_CHANGES for a diff with a TODO and a panic", res.Verdict)
	}
	for _, want := range []string{"TODO", "panic(", "no test file"} {
		if !strings.Contains(res.Review, want) {
			t.Errorf("review does not mention %q:\n%s", want, res.Review)
		}
	}
	if !strings.Contains(res.Review, "auth.go") {
		t.Error("the review should name the changed file")
	}
	for _, heading := range prompt.ReviewHeadings() {
		if !strings.Contains(res.Review, heading) {
			t.Errorf("review is missing the %q heading", heading)
		}
	}

	data, err := os.ReadFile(filepath.Join(rc.WorktreeDir, ".factory-review.md"))
	if err != nil {
		t.Fatalf("read .factory-review.md: %v", err)
	}
	if string(data) != res.Review {
		t.Error("the file and the returned review should match")
	}
}

// The fake reviewer must never approve: a rubber-stamp approval in a PR thread
// would look like a real sign-off from something that read nothing.
func TestFakeRuntimeReviewNeverApproves(t *testing.T) {
	task := api.Task{ID: "rev002", Kind: api.KindReviewPR, RepoOwner: "local", RepoName: "demo"}

	// A spotless diff that even touches tests.
	diff := `diff --git a/sum_test.go b/sum_test.go
--- a/sum_test.go
+++ b/sum_test.go
@@ -1,2 +1,5 @@
 package math
+
+func TestSum(t *testing.T) { if Sum(1, 2) != 3 { t.Fail() } }
`
	promptText := prompt.WithDiff(prompt.ForReview(
		prompt.IssueContext{Repo: "local/demo", Number: 5, Title: "Test sum", Body: "It should be tested."},
		prompt.PRContext{Number: 12},
	), diff)

	res, err := FakeRuntime{}.Run(newRunContext(t, task, promptText))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdict == prompt.VerdictApprove {
		t.Fatal("the fake reviewer must never return APPROVE")
	}
	if res.Verdict != prompt.VerdictComment {
		t.Errorf("verdict = %q, want COMMENT for a clean diff", res.Verdict)
	}
	if !strings.Contains(res.Review, "never returns APPROVE") {
		t.Error("the review should disclose that it is mechanical")
	}
}

func TestNewCommandRuntimeUsesDefaultTemplates(t *testing.T) {
	rt, err := NewCommandRuntime(Claude, "", false)
	if err != nil {
		t.Fatalf("new command runtime: %v", err)
	}
	if rt.Template != DefaultTemplates[Claude].Template {
		t.Errorf("template = %q, want the default", rt.Template)
	}
	if !rt.Stdin {
		t.Error("the claude default should feed the prompt on stdin")
	}

	// An override wins.
	rt, err = NewCommandRuntime(Codex, "my-agent --run {{prompt_file}}", false)
	if err != nil {
		t.Fatalf("new command runtime with override: %v", err)
	}
	if rt.Template != "my-agent --run {{prompt_file}}" {
		t.Errorf("template = %q, want the override", rt.Template)
	}

	// A missing binary must fail with an actionable message.
	missing := &CommandRuntime{RuntimeName: "shell", Template: "definitely-not-a-real-binary-xyz --go"}
	err = missing.Available()
	if err == nil {
		t.Fatal("expected an error for a missing binary")
	}
	if !strings.Contains(err.Error(), "--runtime fake") {
		t.Errorf("the error should suggest the fake runtime, got: %v", err)
	}
}
