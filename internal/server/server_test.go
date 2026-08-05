package server

import (
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/client"
	"github.com/HoaViet-Tech/factory/internal/githubcli"
	"github.com/HoaViet-Tech/factory/internal/store"
)

// newTestServer wires a real store to a real HTTP server and returns a client
// pointed at it. The tests therefore exercise the same code path the CLI and
// the worker use in production.
func newTestServer(t *testing.T, gh *fakeGitHub) (*client.Client, *store.Store) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "server-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := Config{Store: st, DefaultLease: time.Minute}
	if gh != nil {
		cfg.GitHub = gh
	}
	srv := New(cfg)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return client.New(ts.URL), st
}

// fakeGitHub serves canned issues so the ingest path can be tested with no
// network and no credentials.
type fakeGitHub struct {
	issues map[string][]githubcli.Issue // label -> issues
	calls  int
}

func (f *fakeGitHub) ListIssuesByLabel(repo, label string, limit int) ([]githubcli.Issue, error) {
	f.calls++
	return f.issues[label], nil
}

func TestHealth(t *testing.T) {
	c, _ := newTestServer(t, nil)

	got, err := c.Health()
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("status = %v, want ok", got["status"])
	}
}

func TestTaskCreateListShowOverHTTP(t *testing.T) {
	c, _ := newTestServer(t, nil)

	created, err := c.CreateTask(api.CreateTaskRequest{
		Kind:      api.KindManual,
		RepoOwner: "local",
		RepoName:  "demo",
		Title:     "do a thing",
		Prompt:    "please do the thing",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if created.Status != api.StatusQueued {
		t.Errorf("status = %q, want queued", created.Status)
	}

	tasks, err := c.ListTasks("", "", "", 0)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != created.ID {
		t.Fatalf("list returned %+v, want the created task", tasks)
	}

	shown, err := c.GetTask(created.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if shown.Title != "do a thing" {
		t.Errorf("title = %q", shown.Title)
	}

	events, err := c.ListEvents(created.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 {
		t.Error("expected at least the created event")
	}

	if _, err := c.GetTask("missing"); err == nil {
		t.Error("expected an error for an unknown task")
	}

	// Bad input is rejected rather than silently stored.
	if _, err := c.CreateTask(api.CreateTaskRequest{Kind: "bogus", RepoOwner: "a", RepoName: "b", Title: "x"}); err == nil {
		t.Error("expected an error for an invalid kind")
	}
}

func TestWorkerClaimAndCompleteOverHTTP(t *testing.T) {
	c, _ := newTestServer(t, nil)

	// An unregistered worker cannot claim.
	if _, _, err := c.Claim("ghost-worker", 60, nil); err == nil {
		t.Error("an unregistered worker should not be able to claim")
	}

	worker, err := c.RegisterWorker(api.RegisterWorkerRequest{Name: "test", Runtime: "fake"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// An empty queue is a normal, non-error condition.
	if _, _, err := c.Claim(worker.ID, 60, nil); !errors.Is(err, client.ErrNoTask) {
		t.Errorf("claim on an empty queue returned %v, want ErrNoTask", err)
	}

	created, err := c.CreateTask(api.CreateTaskRequest{
		RepoOwner: "local", RepoName: "demo", Title: "claim me", Prompt: "work",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	claimed, lease, err := c.Claim(worker.ID, 60, nil)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ID != created.ID {
		t.Fatalf("claimed %s, want %s", claimed.ID, created.ID)
	}
	if lease == "" {
		t.Fatal("no lease token returned")
	}

	if err := c.AppendEvent(created.ID, lease, api.EventLog, "hello from the worker"); err != nil {
		t.Fatalf("append event: %v", err)
	}

	done, err := c.Complete(created.ID, lease, api.StatusSucceeded, "finished")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.Status != api.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", done.Status)
	}

	events, err := c.ListEvents(created.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var sawLog bool
	for _, e := range events {
		if e.Message == "hello from the worker" {
			sawLog = true
		}
	}
	if !sawLog {
		t.Error("the worker's log line was not recorded")
	}
}

func TestCompleteRejectsWrongLeaseOverHTTP(t *testing.T) {
	c, _ := newTestServer(t, nil)

	worker, err := c.RegisterWorker(api.RegisterWorkerRequest{Name: "test", Runtime: "fake"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	created, err := c.CreateTask(api.CreateTaskRequest{
		RepoOwner: "local", RepoName: "demo", Title: "protected", Prompt: "work",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, _, err := c.Claim(worker.ID, 60, nil); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, err := c.Complete(created.ID, "not-the-token", api.StatusSucceeded, ""); err == nil {
		t.Error("completing with the wrong lease token should fail")
	}
	if err := c.AppendEvent(created.ID, "not-the-token", api.EventLog, "spoofed"); err == nil {
		t.Error("appending an event with the wrong lease token should fail")
	}

	// The task is untouched and still running.
	got, err := c.GetTask(created.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != api.StatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
}

func TestCancelAndRepositoriesOverHTTP(t *testing.T) {
	c, _ := newTestServer(t, nil)

	repo, err := c.AddRepository(api.CreateRepositoryRequest{Owner: "local", Name: "demo", CloneURL: "/tmp/demo"})
	if err != nil {
		t.Fatalf("add repository: %v", err)
	}
	if repo.FullName() != "local/demo" {
		t.Errorf("full name = %q", repo.FullName())
	}

	repos, err := c.ListRepositories()
	if err != nil {
		t.Fatalf("list repositories: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("listed %d repositories, want 1", len(repos))
	}

	created, err := c.CreateTask(api.CreateTaskRequest{
		RepoOwner: "local", RepoName: "demo", Title: "cancel me", Prompt: "work",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	cancelled, err := c.CancelTask(created.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != api.StatusCancelled {
		t.Errorf("status = %q, want cancelled", cancelled.Status)
	}
}

func TestPollIsDisabledWithoutGitHub(t *testing.T) {
	c, _ := newTestServer(t, nil)

	if _, err := c.Poll(); err == nil {
		t.Error("polling without a GitHub client should return a clear error")
	}
}

// TestPollCreatesReviewTasks covers the third pipeline stage: an issue labelled
// factory:review becomes exactly one review_pr task.
func TestPollCreatesReviewTasks(t *testing.T) {
	gh := &fakeGitHub{issues: map[string][]githubcli.Issue{
		"factory:review": {{
			Number: 11,
			Title:  "Add a health endpoint",
			Body:   "The service should expose /healthz returning 200 when it is ready.",
			Labels: []string{"factory:review"},
			Author: "someone",
			URL:    "https://github.com/local/demo/issues/11",
		}},
	}}

	c, _ := newTestServer(t, gh)
	if _, err := c.AddRepository(api.CreateRepositoryRequest{Owner: "local", Name: "demo"}); err != nil {
		t.Fatalf("add repository: %v", err)
	}

	resp, err := c.Poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if resp.TasksCreated != 1 {
		t.Fatalf("created %d tasks, want 1", resp.TasksCreated)
	}

	tasks, err := c.ListTasks("", api.KindReviewPR, "", 0)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("found %d review tasks, want 1", len(tasks))
	}
	if tasks[0].GitHubIssueNumber == nil || *tasks[0].GitHubIssueNumber != 11 {
		t.Errorf("review task is not linked to issue #11: %+v", tasks[0].GitHubIssueNumber)
	}

	// Dedupe applies to reviews too.
	again, err := c.Poll()
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if again.TasksCreated != 0 {
		t.Errorf("second poll created %d tasks, want 0", again.TasksCreated)
	}
}

// TestClaimRoutingOverHTTP proves routing survives the wire, including the
// fallback to the kinds recorded at registration.
func TestClaimRoutingOverHTTP(t *testing.T) {
	c, _ := newTestServer(t, nil)

	reviewer, err := c.RegisterWorker(api.RegisterWorkerRequest{
		Name: "reviewer", Runtime: "fake", Kinds: []string{api.KindReviewPR},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if _, err := c.CreateTask(api.CreateTaskRequest{
		Kind: api.KindImplementTicket, RepoOwner: "local", RepoName: "demo",
		Title: "not for the reviewer", Prompt: "x",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Claiming without asking for kinds must still respect registration.
	if _, _, err := c.Claim(reviewer.ID, 60, nil); !errors.Is(err, client.ErrNoTask) {
		t.Errorf("reviewer claimed an implement task: %v", err)
	}

	review, err := c.CreateTask(api.CreateTaskRequest{
		Kind: api.KindReviewPR, RepoOwner: "local", RepoName: "demo",
		Title: "for the reviewer", Prompt: "x",
	})
	if err != nil {
		t.Fatalf("create review task: %v", err)
	}

	claimed, _, err := c.Claim(reviewer.ID, 60, nil)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ID != review.ID {
		t.Errorf("claimed %q, want the review task", claimed.Title)
	}

	workers, err := c.ListWorkers()
	if err != nil {
		t.Fatalf("list workers: %v", err)
	}
	if len(workers) != 1 || len(workers[0].Kinds) != 1 || workers[0].Kinds[0] != api.KindReviewPR {
		t.Errorf("worker kinds not reported by the API: %+v", workers)
	}
}

func TestPollCreatesOneTaskPerIssue(t *testing.T) {
	gh := &fakeGitHub{issues: map[string][]githubcli.Issue{
		"factory:inbox": {{
			Number: 7,
			Title:  "Login button does nothing",
			Body:   "When I click login on mobile nothing happens. It should open the login form.",
			Labels: []string{"factory:inbox"},
			Author: "someone",
			URL:    "https://github.com/local/demo/issues/7",
		}},
		"factory:ready": {{
			Number: 9,
			Title:  "Add a health endpoint",
			Body:   "The service should expose /healthz returning 200 when it is ready.",
			Labels: []string{"factory:ready"},
			Author: "someone",
			URL:    "https://github.com/local/demo/issues/9",
		}},
	}}

	c, _ := newTestServer(t, gh)

	if _, err := c.AddRepository(api.CreateRepositoryRequest{Owner: "local", Name: "demo"}); err != nil {
		t.Fatalf("add repository: %v", err)
	}

	first, err := c.Poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if first.TasksCreated != 2 {
		t.Fatalf("first poll created %d tasks, want 2", first.TasksCreated)
	}

	// Polling repeatedly must not create anything new.
	for i := 0; i < 3; i++ {
		again, err := c.Poll()
		if err != nil {
			t.Fatalf("repeat poll: %v", err)
		}
		if again.TasksCreated != 0 {
			t.Fatalf("repeat poll created %d tasks, want 0", again.TasksCreated)
		}
		if again.Skipped != 2 {
			t.Errorf("repeat poll skipped %d, want 2", again.Skipped)
		}
	}

	tasks, err := c.ListTasks("", "", "", 0)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("store holds %d tasks, want 2", len(tasks))
	}

	kinds := map[string]bool{}
	for _, task := range tasks {
		kinds[task.Kind] = true
		if task.GitHubIssueNumber == nil {
			t.Errorf("task %s has no issue number", task.ID)
		}
		// The prompt must fence the third-party text.
		if !strings.Contains(task.Prompt, "UNTRUSTED_GITHUB_CONTENT") {
			t.Errorf("task %s prompt does not mark the issue body as untrusted", task.ID)
		}
	}
	if !kinds[api.KindRefineTicket] || !kinds[api.KindImplementTicket] {
		t.Errorf("expected both refine and implement tasks, got %v", kinds)
	}
}
