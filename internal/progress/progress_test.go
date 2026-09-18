package progress

import (
	"strings"
	"testing"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
)

// task builds one task of a run. Every task in a run shares its issue, so only
// the fields the checklist actually reads are set here.
func task(kind, status string, attempts int, milestones ...string) TaskState {
	return TaskState{
		Task: api.Task{
			Kind:         kind,
			Status:       status,
			AttemptCount: attempts,
			CreatedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		Milestones: milestones,
	}
}

func TestComputeStepStates(t *testing.T) {
	refineQueued := task(api.KindRefineTicket, api.StatusQueued, 0)
	refineRunning := task(api.KindRefineTicket, api.StatusRunning, 1)
	refineDone := task(api.KindRefineTicket, api.StatusSucceeded, 1, StepSpecPosted)
	implDone := task(api.KindImplementTicket, api.StatusSucceeded, 1, StepDraftPR)

	tests := []struct {
		name  string
		tasks []TaskState
		want  map[string]State
	}{
		{
			name:  "no tasks means nothing has happened yet",
			tasks: nil,
			want:  map[string]State{StepDetected: Pending, StepRefineStarted: Pending},
		},
		{
			name:  "a queued refine task is a detected ticket, not a started one",
			tasks: []TaskState{refineQueued},
			want:  map[string]State{StepDetected: Done, StepRefineStarted: Pending, StepSpecPosted: Pending},
		},
		{
			name:  "claiming the refine task starts triage",
			tasks: []TaskState{refineRunning},
			want:  map[string]State{StepDetected: Done, StepRefineStarted: Done, StepSpecPosted: Pending},
		},
		{
			// Succeeding is not the same as having posted a spec: a refine task
			// whose issue was closed under it also succeeds.
			name:  "refine succeeds without reporting the milestone",
			tasks: []TaskState{task(api.KindRefineTicket, api.StatusSucceeded, 1)},
			want:  map[string]State{StepRefineStarted: Done, StepSpecPosted: Pending},
		},
		{
			name:  "the milestone is what posts the spec",
			tasks: []TaskState{refineDone},
			want:  map[string]State{StepRefineStarted: Done, StepSpecPosted: Done},
		},
		{
			name:  "a failed refine marks the step it never reached",
			tasks: []TaskState{task(api.KindRefineTicket, api.StatusFailed, 3)},
			want:  map[string]State{StepRefineStarted: Done, StepSpecPosted: Failed, StepAwaitingHuman: Pending},
		},
		{
			// factory:ready goes straight to implementation, so the triage boxes
			// must not sit empty forever.
			name:  "an issue labelled ready skips triage",
			tasks: []TaskState{task(api.KindImplementTicket, api.StatusRunning, 1)},
			want:  map[string]State{StepRefineStarted: Skipped, StepSpecPosted: Skipped, StepImplStarted: Done, StepDraftPR: Pending},
		},
		{
			name:  "a draft PR with nothing left running waits for a human",
			tasks: []TaskState{refineDone, implDone},
			want:  map[string]State{StepDraftPR: Done, StepReviewDone: Pending, StepAwaitingHuman: Done},
		},
		{
			// A review task queued behind the PR means the run is moving again.
			name:  "a queued review takes the run off the human's desk",
			tasks: []TaskState{implDone, task(api.KindReviewPR, api.StatusQueued, 0)},
			want:  map[string]State{StepReviewDone: Pending, StepAwaitingHuman: Pending},
		},
		{
			name:  "a finished review hands the run back to a human",
			tasks: []TaskState{implDone, task(api.KindReviewPR, api.StatusSucceeded, 1, StepReviewDone)},
			want:  map[string]State{StepReviewDone: Done, StepAwaitingHuman: Done},
		},
		{
			// A failure is already the thing a human must look at, so the run
			// must not also claim to be cleanly awaiting approval.
			name:  "a failed implementation does not read as awaiting approval",
			tasks: []TaskState{task(api.KindImplementTicket, api.StatusLost, 3)},
			want:  map[string]State{StepDraftPR: Failed, StepAwaitingHuman: Pending},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Compute("owner/repo", 12, tc.tasks)
			for step, want := range tc.want {
				if got.States[step] != want {
					t.Errorf("step %q = %q, want %q", step, got.States[step], want)
				}
			}
		})
	}
}

func TestRenderMatchesTheChecklistFormat(t *testing.T) {
	got := Compute("HoaViet-Tech/ERP.app", 12, nil).Render()
	want := strings.Join([]string{
		"Factory: HoaViet-Tech/ERP.app#12",
		"[ ] ticket detected",
		"[ ] triage/refine started",
		"[ ] refined spec posted",
		"[ ] implementation started",
		"[ ] draft PR opened",
		"[ ] review completed",
		"[ ] waiting for human approval",
	}, "\n")
	if got != want {
		t.Errorf("render:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestRenderMarkers(t *testing.T) {
	got := Compute("owner/repo", 3, []TaskState{
		task(api.KindImplementTicket, api.StatusFailed, 3),
	}).Render()

	for _, want := range []string{
		"[x] ticket detected",
		"[-] triage/refine started",
		"[x] implementation started",
		"[!] draft PR opened",
		"[ ] waiting for human approval",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q; got:\n%s", want, got)
		}
	}
}

func TestRenderIncludesNeedsHumanAndBlockedNotes(t *testing.T) {
	got := Compute("owner/repo", 3, []TaskState{
		task(api.KindRefineTicket, api.StatusSucceeded, 1,
			StepSpecPosted,
			NoteNeedsHuman+" choose the target chat"),
		task(api.KindImplementTicket, api.StatusSucceeded, 1,
			NoteBlocked+" no files changed"),
	}).Render()

	for _, want := range []string{
		"needs human: choose the target chat",
		"blocked: no files changed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q; got:\n%s", want, got)
		}
	}
}
