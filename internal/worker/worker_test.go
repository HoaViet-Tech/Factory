package worker

import (
	"context"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/client"
	agentruntime "github.com/HoaViet-Tech/factory/internal/runtime"
	"github.com/HoaViet-Tech/factory/internal/server"
	"github.com/HoaViet-Tech/factory/internal/store"
)

// TestWorkerEndToEndWithFakeRuntime is the vertical slice: a real HTTP server,
// a real SQLite database, a real git repository, a real worktree, and the fake
// runtime. No credentials and no network are involved.
func TestWorkerEndToEndWithFakeRuntime(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; skipping the end-to-end worker test")
	}

	sourceRepo := newGitRepo(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	ts := httptest.NewServer(server.New(server.Config{Store: st, DefaultLease: 2 * time.Minute}).Handler())
	defer ts.Close()

	c := client.New(ts.URL)
	if _, err := c.AddRepository(api.CreateRepositoryRequest{
		Owner: "local", Name: "demo", CloneURL: sourceRepo,
	}); err != nil {
		t.Fatalf("add repository: %v", err)
	}

	task, err := c.CreateTask(api.CreateTaskRequest{
		Kind:      api.KindManual,
		RepoOwner: "local",
		RepoName:  "demo",
		Title:     "make a visible change",
		Prompt:    "The worktree should end up with a new file.",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	workDir := t.TempDir()
	w, err := New(Config{
		ServerURL:   ts.URL,
		Name:        "test-worker",
		WorkDir:     workDir,
		Runtime:     agentruntime.FakeRuntime{},
		Once:        true,
		TaskTimeout: 2 * time.Minute,
		Logger:      testLogger(t),
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := w.Run(ctx); err != nil {
		t.Fatalf("worker run: %v", err)
	}

	// 1. The task finished successfully.
	done, err := c.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if done.Status != api.StatusSucceeded {
		events, _ := c.ListEvents(task.ID)
		for _, e := range events {
			t.Logf("event %s: %s", e.Type, e.Message)
		}
		t.Fatalf("task status = %q, want succeeded", done.Status)
	}

	// 2. The worker streamed its logs back to the control plane.
	events, err := c.ListEvents(task.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var sawWorktree, sawRuntime bool
	for _, e := range events {
		if strings.Contains(e.Message, "creating worktree") {
			sawWorktree = true
		}
		if strings.Contains(e.Message, "fake runtime: wrote") {
			sawRuntime = true
		}
	}
	if !sawWorktree {
		t.Error("no worktree creation event was recorded")
	}
	if !sawRuntime {
		t.Error("no runtime output was recorded")
	}

	// 3. The isolated worktree exists and contains the agent's change.
	worktree := filepath.Join(workDir, "worktrees", task.ID)
	if _, err := os.Stat(filepath.Join(worktree, ".factory-task.md")); err != nil {
		t.Errorf("prompt file missing from the worktree: %v", err)
	}
	output := filepath.Join(worktree, "factory-output", task.ID+".md")
	if _, err := os.Stat(output); err != nil {
		t.Errorf("fake runtime output missing: %v", err)
	}

	// 4. The change is on its own branch, and the source repo is untouched.
	branch := gitOutput(t, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if branch != "factory/task-"+task.ID {
		t.Errorf("worktree branch = %q, want factory/task-%s", branch, task.ID)
	}
	if status := gitOutput(t, sourceRepo, "status", "--porcelain"); status != "" {
		t.Errorf("the source repository was modified: %q", status)
	}
}

func TestWorkerFailsClearlyForUnregisteredRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "e2e2.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	ts := httptest.NewServer(server.New(server.Config{Store: st, DefaultLease: time.Minute}).Handler())
	defer ts.Close()

	c := client.New(ts.URL)
	task, err := c.CreateTask(api.CreateTaskRequest{
		RepoOwner: "local", RepoName: "never-registered", Title: "orphan", Prompt: "x",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	w, err := New(Config{
		ServerURL: ts.URL, Name: "test-worker", WorkDir: t.TempDir(),
		Runtime: agentruntime.FakeRuntime{}, Once: true, Logger: testLogger(t),
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("worker run: %v", err)
	}

	done, err := c.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if done.Status != api.StatusFailed {
		t.Errorf("task status = %q, want failed", done.Status)
	}

	events, _ := c.ListEvents(task.ID)
	var explained bool
	for _, e := range events {
		if strings.Contains(e.Message, "is not registered") {
			explained = true
		}
	}
	if !explained {
		t.Error("the failure should explain that the repository is not registered")
	}
}

// newGitRepo creates a small git repository with one commit and returns its
// path, which doubles as a clone URL.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	runGit(t, dir, "config", "user.name", "test")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial commit")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := gitRun(dir, args...); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitRun(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func gitRun(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// testLogger routes worker output into the test log so a failure shows what
// the worker was doing.
func testLogger(t *testing.T) *log.Logger {
	return log.New(testWriter{t}, "[worker] ", 0)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
