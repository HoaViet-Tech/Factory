package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newSourceRepo builds a one-commit repository that can be cloned by path.
func newSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
	} {
		if _, err := Run(dir, nil, args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	if _, err := Run(dir, nil, "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := Run(dir, nil, "commit", "-m", "initial"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	return dir
}

func newWorktree(t *testing.T) *Worktree {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	source := newSourceRepo(t)
	cache := filepath.Join(t.TempDir(), "cache")

	repo, err := EnsureCache(cache, source, nil)
	if err != nil {
		t.Fatalf("ensure cache: %v", err)
	}

	branch, err := repo.DefaultBranch()
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	if branch != "main" {
		t.Errorf("default branch = %q, want main", branch)
	}

	wt, err := repo.AddWorktree(filepath.Join(t.TempDir(), "wt"), "factory/task-test", repo.BaseRef(branch))
	if err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	return wt
}

func TestWorktreeIgnoresFactoryScratchFiles(t *testing.T) {
	wt := newWorktree(t)

	// The prompt file alone must not count as a change, or every task would
	// look like it produced work.
	writeFile(t, wt.Dir, ".factory-task.md", "the prompt")
	changed, err := wt.HasChanges()
	if err != nil {
		t.Fatalf("has changes: %v", err)
	}
	if changed {
		status, _ := wt.Status()
		t.Fatalf("the prompt file should not count as a change, status: %q", status)
	}

	// A real edit does count.
	writeFile(t, wt.Dir, "hello.txt", "hello")
	changed, err = wt.HasChanges()
	if err != nil {
		t.Fatalf("has changes: %v", err)
	}
	if !changed {
		t.Fatal("an agent-created file should count as a change")
	}
}

func TestCommitAllLeavesScratchFilesOut(t *testing.T) {
	wt := newWorktree(t)

	writeFile(t, wt.Dir, ".factory-task.md", "the prompt")
	writeFile(t, wt.Dir, ".factory-refined.md", "the refined ticket")
	writeFile(t, wt.Dir, "hello.txt", "hello")

	if err := wt.CommitAll("test commit", "factory", "factory@example.invalid"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	files, err := wt.ChangedFiles("origin/main")
	if err != nil {
		t.Fatalf("changed files: %v", err)
	}
	joined := strings.Join(files, ",")
	if !strings.Contains(joined, "hello.txt") {
		t.Errorf("the commit should include hello.txt, got %v", files)
	}
	for _, f := range FactoryFiles {
		if strings.Contains(joined, f) {
			t.Errorf("the commit should not include %s, got %v", f, files)
		}
	}
}

func TestAddWorktreeRefusesToClobber(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	source := newSourceRepo(t)
	repo, err := EnsureCache(filepath.Join(t.TempDir(), "cache"), source, nil)
	if err != nil {
		t.Fatalf("ensure cache: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "wt")
	if _, err := repo.AddWorktree(dir, "factory/task-1", repo.BaseRef("main")); err != nil {
		t.Fatalf("first worktree: %v", err)
	}
	if _, err := repo.AddWorktree(dir, "factory/task-2", repo.BaseRef("main")); err == nil {
		t.Error("adding a worktree over an existing directory should fail loudly")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
