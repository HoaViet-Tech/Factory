package store

import (
	"errors"
	"testing"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
)

// mustCreateIssueTask enqueues one GitHub-triggered task, the way the poller
// does.
func mustCreateIssueTask(t *testing.T, st *Store, kind string, issue int) api.Task {
	t.Helper()
	task, created, err := st.CreateTaskForIssue(DedupeKey("local", "demo", issue, kind), api.CreateTaskRequest{
		Kind:              kind,
		RepoOwner:         "local",
		RepoName:          "demo",
		GitHubIssueNumber: &issue,
		Title:             "work on it",
		Prompt:            "do the thing",
	})
	if err != nil {
		t.Fatalf("create task for issue #%d: %v", issue, err)
	}
	if !created {
		t.Fatalf("task for %s #%d already existed", kind, issue)
	}
	return task
}

func TestListRecentRunsGroupsTasksByIssue(t *testing.T) {
	st := newTestStore(t)

	// Two stages of the same issue are one run; a different issue is another.
	mustCreateIssueTask(t, st, api.KindRefineTicket, 7)
	mustCreateIssueTask(t, st, api.KindImplementTicket, 7)
	mustCreateIssueTask(t, st, api.KindRefineTicket, 8)
	// A task with no issue attached is not a run at all.
	mustCreateTask(t, st, "manual, no issue")

	runs, err := st.ListRecentRuns(time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("list recent runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs (%+v), want one per issue", len(runs), runs)
	}

	seen := map[int]bool{}
	for _, r := range runs {
		if r.FullName() != "local/demo" {
			t.Errorf("run repo = %q, want local/demo", r.FullName())
		}
		seen[r.IssueNumber] = true
	}
	if !seen[7] || !seen[8] {
		t.Errorf("runs = %+v, want issues 7 and 8", runs)
	}

	tasks, err := st.TasksForIssue(RunRef{Owner: "local", Name: "demo", IssueNumber: 7})
	if err != nil {
		t.Fatalf("tasks for issue: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("issue 7 has %d tasks, want both stages", len(tasks))
	}
}

// A run that has not moved in a long time cannot change again, so refreshing it
// would only burn queries.
func TestListRecentRunsSkipsRunsOutsideTheWindow(t *testing.T) {
	st := newTestStore(t)

	old := time.Now().UTC().Add(-48 * time.Hour)
	st.SetClock(func() time.Time { return old })
	mustCreateIssueTask(t, st, api.KindRefineTicket, 7)

	st.SetClock(func() time.Time { return time.Now().UTC() })
	mustCreateIssueTask(t, st, api.KindRefineTicket, 8)

	runs, err := st.ListRecentRuns(time.Now().UTC().Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("list recent runs: %v", err)
	}
	if len(runs) != 1 || runs[0].IssueNumber != 8 {
		t.Fatalf("runs = %+v, want only issue 8", runs)
	}
}

func TestRunNotificationRoundTrip(t *testing.T) {
	st := newTestStore(t)
	run := RunRef{Owner: "local", Name: "demo", IssueNumber: 7}

	if _, err := st.GetRunNotification(run, "fake:chat"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get before save = %v, want ErrNotFound", err)
	}

	if err := st.SaveRunNotification(run, "fake:chat", RunNotification{ExternalID: "42", LastText: "first"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.GetRunNotification(run, "fake:chat")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExternalID != "42" || got.LastText != "first" {
		t.Errorf("got %+v, want {42 first}", got)
	}

	// Saving again updates the same row rather than adding a second message.
	if err := st.SaveRunNotification(run, "fake:chat", RunNotification{ExternalID: "42", LastText: "second"}); err != nil {
		t.Fatalf("resave: %v", err)
	}
	got, err = st.GetRunNotification(run, "fake:chat")
	if err != nil {
		t.Fatalf("get after resave: %v", err)
	}
	if got.LastText != "second" {
		t.Errorf("last text = %q, want second", got.LastText)
	}

	// A different chat is a different message for the same run.
	if _, err := st.GetRunNotification(run, "other:chat"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get for another channel = %v, want ErrNotFound", err)
	}
}

func TestListTaskEventsOfTypeFiltersTheLog(t *testing.T) {
	st := newTestStore(t)
	task := mustCreateTask(t, st, "milestone test")

	if err := st.AppendEvent(task.ID, api.EventLog, "noisy agent output"); err != nil {
		t.Fatalf("append log: %v", err)
	}
	if err := st.AppendEvent(task.ID, api.EventMilestone, "draft PR opened"); err != nil {
		t.Fatalf("append milestone: %v", err)
	}

	events, err := st.ListTaskEventsOfType(task.ID, api.EventMilestone)
	if err != nil {
		t.Fatalf("list milestones: %v", err)
	}
	if len(events) != 1 || events[0].Message != "draft PR opened" {
		t.Fatalf("got %+v, want only the milestone", events)
	}
}
