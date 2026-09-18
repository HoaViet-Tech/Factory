// Package progress turns one GitHub issue's factory tasks into the checklist a
// human watches while the run happens.
//
// The checklist is derived, never stored. Every step is a question answered
// from the task rows and their milestone events, so a restarted control plane,
// a reaped lease or a deleted chat message all recover by simply recomputing.
// The only thing that *is* persisted is the id of the message we already sent,
// so the next update edits it instead of posting a new one.
package progress

import (
	"errors"
	"fmt"
	"strings"

	"github.com/HoaViet-Tech/factory/internal/api"
)

// The checklist steps, in the order they are rendered.
//
// These strings are both the identity of a step and the text a human reads, so
// the vocabulary in the code is the vocabulary in the chat message.
//
// StepSpecPosted, StepDraftPR and StepReviewDone are reported by the worker as
// api.EventMilestone events, because only the worker knows whether the side
// effect actually happened. The rest are derived from task state.
const (
	StepDetected      = "ticket detected"
	StepRefineStarted = "triage/refine started"
	StepSpecPosted    = "refined spec posted"
	StepImplStarted   = "implementation started"
	StepDraftPR       = "draft PR opened"
	StepReviewDone    = "review completed"
	StepAwaitingHuman = "waiting for human approval"

	NoteNeedsHuman = "needs human:"
	NoteBlocked    = "blocked:"
)

// Order returns the checklist top to bottom.
func Order() []string {
	return []string{
		StepDetected, StepRefineStarted, StepSpecPosted, StepImplStarted,
		StepDraftPR, StepReviewDone, StepAwaitingHuman,
	}
}

// State is what has happened to one step.
type State string

const (
	Pending State = "pending"
	Done    State = "done"
	Failed  State = "failed"
	// Skipped is for a stage this run never goes through — an issue labelled
	// factory:ready straight away is implemented without ever being refined,
	// and leaving those boxes empty forever would read as "stuck".
	Skipped State = "skipped"
)

// marker is the character inside the checkbox.
func marker(s State) string {
	switch s {
	case Done:
		return "x"
	case Failed:
		return "!"
	case Skipped:
		return "-"
	default:
		return " "
	}
}

// TaskState is one task of a run together with the milestones it reported.
//
// Only api.EventMilestone messages belong in Milestones. Passing a task's full
// event log would work but would drag every agent log line through the
// checklist computation on every tick.
type TaskState struct {
	Task       api.Task
	Milestones []string
}

// Run is one issue's checklist.
type Run struct {
	Repo   string // "owner/name"
	Issue  int
	States map[string]State
	Notes  []string
}

// Compute answers every checklist step from the run's tasks.
func Compute(repo string, issue int, tasks []TaskState) Run {
	r := Run{Repo: repo, Issue: issue, States: make(map[string]State, len(Order()))}
	for _, step := range Order() {
		r.States[step] = Pending
	}
	if len(tasks) == 0 {
		return r
	}
	r.States[StepDetected] = Done

	// The dedupe key gives one task per kind per issue, but a newest-wins merge
	// keeps the checklist honest if that ever stops being true.
	byKind := make(map[string]TaskState, len(tasks))
	reached := make(map[string]bool)
	busy := false
	for _, ts := range tasks {
		if prev, ok := byKind[ts.Task.Kind]; !ok || ts.Task.CreatedAt.After(prev.Task.CreatedAt) {
			byKind[ts.Task.Kind] = ts
		}
		for _, m := range ts.Milestones {
			reached[m] = true
			if isNote(m) {
				r.Notes = append(r.Notes, m)
			}
		}
		if !api.IsTerminal(ts.Task.Status) {
			busy = true
		}
	}

	// stage fills in one "finished" box, plus the "started" box in front of it
	// when the stage has one. The finished box is only ticked by a milestone
	// the worker actually reported: a task can succeed having deliberately done
	// nothing (the issue was closed, the label was removed), and that must not
	// look like a posted spec or an opened PR.
	stage := func(kind, startStep, doneStep string) {
		ts, ok := byKind[kind]
		if !ok {
			return
		}
		if startStep != "" && (ts.Task.AttemptCount > 0 || ts.Task.Status != api.StatusQueued) {
			r.States[startStep] = Done
		}
		switch {
		case reached[doneStep]:
			r.States[doneStep] = Done
		case api.IsTerminal(ts.Task.Status) && ts.Task.Status != api.StatusSucceeded:
			r.States[doneStep] = Failed
		}
	}

	stage(api.KindRefineTicket, StepRefineStarted, StepSpecPosted)
	stage(api.KindImplementTicket, StepImplStarted, StepDraftPR)
	stage(api.KindReviewPR, "", StepReviewDone) // the review stage has no separate "started" box

	// An issue labelled factory:ready never passes through triage.
	if _, refined := byKind[api.KindRefineTicket]; !refined {
		r.States[StepRefineStarted] = Skipped
		r.States[StepSpecPosted] = Skipped
	}

	// "Waiting for a human" means the machine has stopped: implementation ran,
	// nothing is queued or in flight, and no step went wrong. A failed step is
	// waiting for a human too, but the failure marker already says so and is
	// the more useful thing to show.
	if r.States[StepImplStarted] == Done && !busy && !r.anyFailed() {
		r.States[StepAwaitingHuman] = Done
	}
	return r
}

func isNote(m string) bool {
	m = strings.ToLower(strings.TrimSpace(m))
	return strings.HasPrefix(m, NoteNeedsHuman) || strings.HasPrefix(m, NoteBlocked)
}

func (r Run) anyFailed() bool {
	for _, s := range r.States {
		if s == Failed {
			return true
		}
	}
	return false
}

// Render formats the checklist as the plain-text message a chat client shows.
func (r Run) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Factory: %s#%d", r.Repo, r.Issue)
	for _, step := range Order() {
		fmt.Fprintf(&b, "\n[%s] %s", marker(r.States[step]), step)
	}
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "\n%s", note)
	}
	return b.String()
}

// ErrMessageGone means the message a Notifier was asked to edit no longer
// exists — somebody deleted it in the chat. The caller's correct response is to
// send a fresh one rather than to retry the edit forever.
var ErrMessageGone = errors.New("progress message no longer exists")

// Notifier publishes a run checklist somewhere a human is watching and can
// update what it published in place.
type Notifier interface {
	// Channel identifies the destination. It is stored alongside the message
	// id so an id is never replayed against a different chat.
	Channel() string
	// Send posts a new message and returns the id needed to edit it later.
	Send(text string) (externalID string, err error)
	// Edit replaces the text of a message Send returned. It returns an error
	// wrapping ErrMessageGone when that message is no longer there.
	Edit(externalID, text string) error
}
