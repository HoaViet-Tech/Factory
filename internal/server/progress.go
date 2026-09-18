package server

import (
	"context"
	"errors"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/progress"
	"github.com/HoaViet-Tech/factory/internal/store"
)

// progressWindow is how far back a run is still refreshed.
//
// A run that has not been touched in a day is over; its checklist cannot change
// again, so recomputing it every tick would only burn queries.
const progressWindow = 24 * time.Hour

// progressLoop keeps one live checklist message per run up to date.
//
// It is a loop over stored state rather than a hook on each write, which means
// the chat catches up by itself after a restart, a crash, or a spell with no
// network — the next tick republishes whatever the database now says.
func (s *Server) progressLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.ProgressInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.publishProgress()
		}
	}
}

// publishProgress refreshes the checklist of every recently active run.
//
// The mutex matters: a poll that just created tasks nudges this directly, and
// without serialisation that nudge could race the ticker into sending two first
// messages for the same run.
func (s *Server) publishProgress() {
	if s.cfg.Notifier == nil {
		return
	}
	s.progressMu.Lock()
	defer s.progressMu.Unlock()

	runs, err := s.store.ListRecentRuns(s.store.Now().Add(-progressWindow))
	if err != nil {
		s.logger.Printf("progress: list runs: %v", err)
		return
	}
	channel := s.cfg.Notifier.Channel()
	for _, run := range runs {
		// One unreachable chat or one bad row must not stop the other runs
		// from being updated, so failures are logged and stepped over.
		if err := s.publishRun(run, channel); err != nil {
			s.logger.Printf("progress: %s#%d: %v", run.FullName(), run.IssueNumber, err)
		}
	}
}

// publishRun recomputes one run's checklist and publishes it if it changed.
func (s *Server) publishRun(run store.RunRef, channel string) error {
	text, err := s.renderRun(run)
	if err != nil {
		return err
	}

	published, err := s.store.GetRunNotification(run, channel)
	switch {
	case errors.Is(err, store.ErrNotFound):
		id, err := s.cfg.Notifier.Send(text)
		if err != nil {
			return err
		}
		return s.store.SaveRunNotification(run, channel, store.RunNotification{ExternalID: id, LastText: text})
	case err != nil:
		return err
	case published.LastText == text:
		// Nothing moved since the last tick. Saying so again would cost an API
		// call and, on Telegram, be rejected as an unmodified edit anyway.
		return nil
	}

	if err := s.cfg.Notifier.Edit(published.ExternalID, text); err != nil {
		if !errors.Is(err, progress.ErrMessageGone) {
			// Leave the stored text alone so the next tick retries this edit.
			return err
		}
		// Somebody deleted the message in the chat. Start a new one rather than
		// retrying an edit that can never succeed.
		id, sendErr := s.cfg.Notifier.Send(text)
		if sendErr != nil {
			return sendErr
		}
		published.ExternalID = id
	}
	published.LastText = text
	return s.store.SaveRunNotification(run, channel, published)
}

// renderRun builds the checklist text for one run.
func (s *Server) renderRun(run store.RunRef) (string, error) {
	tasks, err := s.store.TasksForIssue(run)
	if err != nil {
		return "", err
	}

	states := make([]progress.TaskState, 0, len(tasks))
	for _, t := range tasks {
		events, err := s.store.ListTaskEventsOfType(t.ID, api.EventMilestone)
		if err != nil {
			return "", err
		}
		milestones := make([]string, 0, len(events))
		for _, e := range events {
			milestones = append(milestones, e.Message)
		}
		states = append(states, progress.TaskState{Task: t, Milestones: milestones})
	}
	return progress.Compute(run.FullName(), run.IssueNumber, states).Render(), nil
}
