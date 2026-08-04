package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
)

// newTestStore opens a throwaway database on disk. A file (rather than
// :memory:) keeps behaviour identical to production.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustCreateTask(t *testing.T, st *Store, title string) api.Task {
	t.Helper()
	task, err := st.CreateTask(api.CreateTaskRequest{
		Kind:      api.KindManual,
		RepoOwner: "local",
		RepoName:  "demo",
		Title:     title,
		Prompt:    "do the thing",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

func mustRegisterWorker(t *testing.T, st *Store) api.Worker {
	t.Helper()
	w, err := st.RegisterWorker(api.RegisterWorkerRequest{Name: "test", Runtime: "fake"})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	return w
}

func TestCreateListAndGetTask(t *testing.T) {
	st := newTestStore(t)

	created := mustCreateTask(t, st, "first task")
	if created.Status != api.StatusQueued {
		t.Fatalf("new task status = %q, want %q", created.Status, api.StatusQueued)
	}
	if created.ID == "" {
		t.Fatal("new task has no ID")
	}

	got, err := st.GetTask(created.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Title != "first task" {
		t.Errorf("title = %q, want %q", got.Title, "first task")
	}

	mustCreateTask(t, st, "second task")

	all, err := st.ListTasks(TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("listed %d tasks, want 2", len(all))
	}

	queued, err := st.ListTasks(TaskFilter{Status: api.StatusQueued})
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	if len(queued) != 2 {
		t.Errorf("listed %d queued tasks, want 2", len(queued))
	}

	byRepo, err := st.ListTasks(TaskFilter{Repo: "local/demo"})
	if err != nil {
		t.Fatalf("list by repo: %v", err)
	}
	if len(byRepo) != 2 {
		t.Errorf("listed %d tasks for local/demo, want 2", len(byRepo))
	}

	if _, err := st.GetTask("does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTask on unknown id returned %v, want ErrNotFound", err)
	}

	// A "created" event should exist, which is what `task show` displays.
	events, err := st.ListTaskEvents(created.ID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 || events[0].Type != api.EventCreated {
		t.Errorf("expected a %q event, got %+v", api.EventCreated, events)
	}
}

func TestCreateTaskValidation(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.CreateTask(api.CreateTaskRequest{Kind: "nonsense", RepoOwner: "a", RepoName: "b", Title: "x"}); err == nil {
		t.Error("expected an error for an invalid kind")
	}
	if _, err := st.CreateTask(api.CreateTaskRequest{RepoOwner: "a", RepoName: "b"}); err == nil {
		t.Error("expected an error for a missing title")
	}
	if _, err := st.CreateTask(api.CreateTaskRequest{Title: "x"}); err == nil {
		t.Error("expected an error for a missing repository")
	}
}

func TestClaimAndComplete(t *testing.T) {
	st := newTestStore(t)
	worker := mustRegisterWorker(t, st)
	created := mustCreateTask(t, st, "claim me")

	claimed, token, err := st.ClaimTask(worker.ID, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ID != created.ID {
		t.Fatalf("claimed %s, want %s", claimed.ID, created.ID)
	}
	if claimed.Status != api.StatusRunning {
		t.Errorf("claimed status = %q, want %q", claimed.Status, api.StatusRunning)
	}
	if claimed.AttemptCount != 1 {
		t.Errorf("attempt count = %d, want 1", claimed.AttemptCount)
	}
	if token == "" {
		t.Fatal("claim returned an empty lease token")
	}

	// The queue is now empty.
	if _, _, err := st.ClaimTask(worker.ID, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Errorf("second claim returned %v, want ErrNotFound", err)
	}

	done, err := st.CompleteTask(created.ID, token, api.StatusSucceeded, "all good")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.Status != api.StatusSucceeded {
		t.Errorf("status = %q, want %q", done.Status, api.StatusSucceeded)
	}
	if done.LeaseToken != nil {
		t.Error("lease token should be cleared once the task is complete")
	}
}

func TestCompleteRequiresValidLeaseToken(t *testing.T) {
	st := newTestStore(t)
	worker := mustRegisterWorker(t, st)
	created := mustCreateTask(t, st, "lease me")

	_, token, err := st.ClaimTask(worker.ID, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, err := st.CompleteTask(created.ID, "wrong-token", api.StatusSucceeded, ""); !errors.Is(err, ErrInvalidLease) {
		t.Errorf("completing with a wrong token returned %v, want ErrInvalidLease", err)
	}
	if _, err := st.CompleteTask(created.ID, "", api.StatusSucceeded, ""); !errors.Is(err, ErrInvalidLease) {
		t.Errorf("completing with an empty token returned %v, want ErrInvalidLease", err)
	}

	// Appending events is protected by the same check.
	if err := st.AppendTaskEventWithLease(created.ID, "wrong-token", api.EventLog, "hi"); !errors.Is(err, ErrInvalidLease) {
		t.Errorf("appending with a wrong token returned %v, want ErrInvalidLease", err)
	}

	// The real token still works, proving the task was never corrupted.
	if _, err := st.CompleteTask(created.ID, token, api.StatusSucceeded, ""); err != nil {
		t.Fatalf("completing with the correct token failed: %v", err)
	}

	// A finished task cannot be completed twice.
	if _, err := st.CompleteTask(created.ID, token, api.StatusSucceeded, ""); !errors.Is(err, ErrNotRunning) {
		t.Errorf("second completion returned %v, want ErrNotRunning", err)
	}
}

func TestExpiredLeaseIsRequeuedThenLost(t *testing.T) {
	st := newTestStore(t)
	worker := mustRegisterWorker(t, st)
	created := mustCreateTask(t, st, "abandoned task")

	// A controllable clock lets the test expire leases without sleeping.
	now := time.Now().UTC()
	st.SetClock(func() time.Time { return now })

	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		claimed, _, err := st.ClaimTask(worker.ID, time.Minute)
		if err != nil {
			t.Fatalf("claim %d: %v", attempt, err)
		}
		if claimed.AttemptCount != attempt {
			t.Fatalf("attempt count = %d, want %d", claimed.AttemptCount, attempt)
		}

		// The worker dies: time passes and the lease expires.
		now = now.Add(2 * time.Minute)

		reaped, err := st.ReapExpiredLeases()
		if err != nil {
			t.Fatalf("reap %d: %v", attempt, err)
		}
		if len(reaped) != 1 {
			t.Fatalf("reaped %d tasks, want 1", len(reaped))
		}

		got, err := st.GetTask(created.ID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.WorkerID != nil || got.LeaseToken != nil {
			t.Error("reaping should clear the worker and lease token")
		}

		want := api.StatusQueued
		if attempt == MaxAttempts {
			want = api.StatusLost
		}
		if got.Status != want {
			t.Fatalf("after attempt %d status = %q, want %q", attempt, got.Status, want)
		}
	}

	// A lost task is terminal, so nothing claims it again.
	if _, _, err := st.ClaimTask(worker.ID, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Errorf("claim after lost returned %v, want ErrNotFound", err)
	}
}

func TestLeaseNotExpiredIsLeftAlone(t *testing.T) {
	st := newTestStore(t)
	worker := mustRegisterWorker(t, st)
	created := mustCreateTask(t, st, "still working")

	if _, _, err := st.ClaimTask(worker.ID, time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	reaped, err := st.ReapExpiredLeases()
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("reaped %d tasks, want 0", len(reaped))
	}

	got, _ := st.GetTask(created.ID)
	if got.Status != api.StatusRunning {
		t.Errorf("status = %q, want %q", got.Status, api.StatusRunning)
	}
}

func TestGitHubDedupeKeyCreatesOnlyOneTask(t *testing.T) {
	st := newTestStore(t)

	issue := 42
	key := DedupeKey("local", "demo", issue, api.KindRefineTicket)
	req := api.CreateTaskRequest{
		Kind:              api.KindRefineTicket,
		RepoOwner:         "local",
		RepoName:          "demo",
		GitHubIssueNumber: &issue,
		Title:             "Refine #42",
		Prompt:            "refine it",
	}

	first, created, err := st.CreateTaskForIssue(key, req)
	if err != nil {
		t.Fatalf("first CreateTaskForIssue: %v", err)
	}
	if !created {
		t.Fatal("the first observation should have created a task")
	}

	// Polling is at-least-once: the same issue is seen again and again.
	for i := 0; i < 5; i++ {
		again, created, err := st.CreateTaskForIssue(key, req)
		if err != nil {
			t.Fatalf("repeat CreateTaskForIssue: %v", err)
		}
		if created {
			t.Fatal("a repeated poll must not create a second task")
		}
		if again.ID != first.ID {
			t.Fatalf("repeat returned task %s, want the original %s", again.ID, first.ID)
		}
	}

	tasks, err := st.ListTasks(TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("store holds %d tasks, want exactly 1", len(tasks))
	}

	// A different workflow for the same issue is a different key, so the
	// implement stage is still allowed to run later.
	implementKey := DedupeKey("local", "demo", issue, api.KindImplementTicket)
	implReq := req
	implReq.Kind = api.KindImplementTicket
	implReq.Title = "Implement #42"

	if _, created, err := st.CreateTaskForIssue(implementKey, implReq); err != nil {
		t.Fatalf("implement CreateTaskForIssue: %v", err)
	} else if !created {
		t.Fatal("a different workflow kind should create its own task")
	}

	obs, err := st.ListObservations()
	if err != nil {
		t.Fatalf("list observations: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("recorded %d observations, want 2", len(obs))
	}
}

func TestCancelTask(t *testing.T) {
	st := newTestStore(t)
	created := mustCreateTask(t, st, "cancel me")

	cancelled, err := st.CancelTask(created.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != api.StatusCancelled {
		t.Errorf("status = %q, want %q", cancelled.Status, api.StatusCancelled)
	}
	if _, err := st.CancelTask(created.ID); !errors.Is(err, ErrTerminal) {
		t.Errorf("cancelling twice returned %v, want ErrTerminal", err)
	}
}

func TestRepositoriesAndWorkers(t *testing.T) {
	st := newTestStore(t)

	repo, err := st.AddRepository(api.CreateRepositoryRequest{Owner: "local", Name: "demo"})
	if err != nil {
		t.Fatalf("add repository: %v", err)
	}
	if repo.CloneURL != "https://github.com/local/demo.git" {
		t.Errorf("default clone url = %q", repo.CloneURL)
	}
	if repo.LocalCachePath != "repos/local/demo" {
		t.Errorf("cache path = %q, want repos/local/demo", repo.LocalCachePath)
	}

	// Re-adding updates instead of duplicating.
	if _, err := st.AddRepository(api.CreateRepositoryRequest{Owner: "local", Name: "demo", CloneURL: "/tmp/demo"}); err != nil {
		t.Fatalf("re-add repository: %v", err)
	}
	repos, err := st.ListRepositories(false)
	if err != nil {
		t.Fatalf("list repositories: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("stored %d repositories, want 1", len(repos))
	}
	if repos[0].CloneURL != "/tmp/demo" {
		t.Errorf("clone url = %q, want /tmp/demo", repos[0].CloneURL)
	}

	// A worker keeps its identity across re-registration.
	w1 := mustRegisterWorker(t, st)
	w2, err := st.RegisterWorker(api.RegisterWorkerRequest{ID: w1.ID, Name: "test", Runtime: "fake"})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if w2.ID != w1.ID {
		t.Errorf("worker id changed on re-register: %s -> %s", w1.ID, w2.ID)
	}
	workers, err := st.ListWorkers()
	if err != nil {
		t.Fatalf("list workers: %v", err)
	}
	if len(workers) != 1 {
		t.Errorf("stored %d workers, want 1", len(workers))
	}

	if _, err := st.Heartbeat("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("heartbeat for unknown worker returned %v, want ErrNotFound", err)
	}
}
