package server

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/progress"
	"github.com/HoaViet-Tech/factory/internal/store"
)

// fakeNotifier stands in for a chat. It records what was published so a test
// can assert on both the content and the send-once-then-edit behaviour.
type fakeNotifier struct {
	sent  []string
	edits []string
	next  int
	// gone makes the next Edit report that the message was deleted.
	gone bool
}

func (f *fakeNotifier) Channel() string { return "fake:chat" }

func (f *fakeNotifier) Send(text string) (string, error) {
	f.next++
	f.sent = append(f.sent, text)
	return strconv.Itoa(f.next), nil
}

func (f *fakeNotifier) Edit(externalID, text string) error {
	if f.gone {
		f.gone = false
		return fmt.Errorf("%w: somebody deleted it", progress.ErrMessageGone)
	}
	f.edits = append(f.edits, externalID+": "+text)
	return nil
}

// last returns the most recent published text, whether sent or edited.
func (f *fakeNotifier) last() string {
	if len(f.edits) > 0 {
		return f.edits[len(f.edits)-1]
	}
	if len(f.sent) > 0 {
		return f.sent[len(f.sent)-1]
	}
	return ""
}

// newProgressServer wires a real store to a server with a fake chat attached.
func newProgressServer(t *testing.T) (*Server, *store.Store, *fakeNotifier) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "progress-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	n := &fakeNotifier{}
	return New(Config{Store: st, DefaultLease: time.Minute, Notifier: n}), st, n
}

// createIssueTask enqueues one GitHub-triggered task the way the poller does.
func createIssueTask(t *testing.T, st *store.Store, kind string, issue int) api.Task {
	t.Helper()
	task, created, err := st.CreateTaskForIssue(
		store.DedupeKey("owner", "repo", issue, kind),
		api.CreateTaskRequest{
			Kind:              kind,
			RepoOwner:         "owner",
			RepoName:          "repo",
			GitHubIssueNumber: &issue,
			Title:             "Refine #1: something",
			Prompt:            "do the thing",
		})
	if err != nil {
		t.Fatalf("create task for issue: %v", err)
	}
	if !created {
		t.Fatalf("task for %s #%d already existed", kind, issue)
	}
	return task
}

func TestProgressSendsOnceThenEditsInPlace(t *testing.T) {
	srv, st, chat := newProgressServer(t)

	createIssueTask(t, st, api.KindRefineTicket, 12)

	srv.publishProgress()
	if len(chat.sent) != 1 || len(chat.edits) != 0 {
		t.Fatalf("after detection: %d sent, %d edited; want 1 sent", len(chat.sent), len(chat.edits))
	}
	if !strings.Contains(chat.sent[0], "Factory: owner/repo#12") {
		t.Errorf("message does not name the run:\n%s", chat.sent[0])
	}
	if !strings.Contains(chat.sent[0], "[x] ticket detected") {
		t.Errorf("message does not tick detection:\n%s", chat.sent[0])
	}
	if !strings.Contains(chat.sent[0], "[ ] refined spec posted") {
		t.Errorf("message ticked a step that has not happened:\n%s", chat.sent[0])
	}

	// Nothing moved, so a second pass must not cost a chat update.
	srv.publishProgress()
	if len(chat.sent) != 1 || len(chat.edits) != 0 {
		t.Fatalf("an unchanged run published again: %d sent, %d edited", len(chat.sent), len(chat.edits))
	}

	claimed, lease, err := st.ClaimTask("worker-1", time.Minute, nil)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	srv.publishProgress()
	if len(chat.sent) != 1 || len(chat.edits) != 1 {
		t.Fatalf("after claim: %d sent, %d edited; want 1 sent and 1 edited", len(chat.sent), len(chat.edits))
	}
	if !strings.HasPrefix(chat.edits[0], "1: ") {
		t.Errorf("edit targeted %q, want the message that was sent first", chat.edits[0])
	}
	if !strings.Contains(chat.last(), "[x] triage/refine started") {
		t.Errorf("message does not show triage running:\n%s", chat.last())
	}

	// The worker reporting a milestone is what ticks a side-effect step.
	if err := st.AppendTaskEventWithLease(claimed.ID, lease, api.EventMilestone, progress.StepSpecPosted); err != nil {
		t.Fatalf("append milestone: %v", err)
	}
	srv.publishProgress()
	if len(chat.sent) != 1 || len(chat.edits) != 2 {
		t.Fatalf("after the milestone: %d sent, %d edited; want 1 sent and 2 edited", len(chat.sent), len(chat.edits))
	}
	if !strings.Contains(chat.last(), "[x] refined spec posted") {
		t.Errorf("message does not show the posted spec:\n%s", chat.last())
	}
}

// Every issue gets its own message, so two runs never overwrite each other.
func TestProgressKeepsOneMessagePerRun(t *testing.T) {
	srv, st, chat := newProgressServer(t)

	createIssueTask(t, st, api.KindRefineTicket, 12)
	createIssueTask(t, st, api.KindImplementTicket, 13)

	srv.publishProgress()
	if len(chat.sent) != 2 {
		t.Fatalf("sent %d messages, want one per run", len(chat.sent))
	}
	joined := strings.Join(chat.sent, "\n---\n")
	for _, want := range []string{"Factory: owner/repo#12", "Factory: owner/repo#13"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no message for %q:\n%s", want, joined)
		}
	}
}

// Retrying an edit against a deleted message can never succeed, so the run
// starts a new message instead.
func TestProgressResendsWhenTheMessageWasDeleted(t *testing.T) {
	srv, st, chat := newProgressServer(t)

	createIssueTask(t, st, api.KindRefineTicket, 12)
	srv.publishProgress()

	claimed, lease, err := st.ClaimTask("worker-1", time.Minute, nil)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	chat.gone = true
	srv.publishProgress()

	if len(chat.sent) != 2 {
		t.Fatalf("sent %d messages, want a replacement for the deleted one", len(chat.sent))
	}
	if !strings.Contains(chat.sent[1], "[x] triage/refine started") {
		t.Errorf("replacement message is not up to date:\n%s", chat.sent[1])
	}

	// The replacement's id, not the dead one, is what later updates must edit.
	if err := st.AppendTaskEventWithLease(claimed.ID, lease, api.EventMilestone, progress.StepSpecPosted); err != nil {
		t.Fatalf("append milestone: %v", err)
	}
	srv.publishProgress()
	if len(chat.sent) != 2 {
		t.Fatalf("sent %d messages, want no further resends", len(chat.sent))
	}
	if len(chat.edits) != 1 || !strings.HasPrefix(chat.edits[0], "2: ") {
		t.Errorf("edits = %v, want one edit of the replacement message", chat.edits)
	}
}

// Without a notifier the control plane must simply do nothing, not panic.
func TestProgressIsANoOpWithoutANotifier(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "progress-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv := New(Config{Store: st, DefaultLease: time.Minute})
	createIssueTask(t, st, api.KindRefineTicket, 12)
	srv.publishProgress()
}
