package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
)

// RunRef identifies one GitHub issue's trip through the factory.
//
// A run is not a row. It is every task created for one issue — refine, then
// implement, then review — which is the unit a human actually watches, and the
// unit the live checklist is rendered for.
type RunRef struct {
	Owner       string
	Name        string
	IssueNumber int
}

// FullName returns "owner/name".
func (r RunRef) FullName() string { return r.Owner + "/" + r.Name }

// ListRecentRuns returns the runs whose newest task was touched at or after
// `since`, most recently touched first.
//
// The window is what keeps the progress loop's cost flat: a run that finished
// last week cannot change, so recomputing its checklist every few seconds only
// burns queries.
func (s *Store) ListRecentRuns(since time.Time) ([]RunRef, error) {
	rows, err := s.db.Query(`
SELECT repo_owner, repo_name, github_issue_number
FROM tasks
WHERE github_issue_number IS NOT NULL
GROUP BY repo_owner, repo_name, github_issue_number
HAVING MAX(updated_at) >= ?
ORDER BY MAX(updated_at) DESC`, formatTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []RunRef{}
	for rows.Next() {
		var r RunRef
		if err := rows.Scan(&r.Owner, &r.Name, &r.IssueNumber); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// TasksForIssue returns every task created for one issue, oldest first.
func (s *Store) TasksForIssue(r RunRef) ([]api.Task, error) {
	rows, err := s.db.Query(`SELECT `+taskColumns+` FROM tasks
		WHERE repo_owner = ? AND repo_name = ? AND github_issue_number = ?
		ORDER BY created_at ASC, id ASC`, r.Owner, r.Name, r.IssueNumber)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tasks := []api.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// RunNotification is the chat message the control plane keeps up to date for
// one run.
type RunNotification struct {
	// ExternalID is the provider's message id, used to edit rather than repost.
	ExternalID string
	// LastText is what was last published. Comparing against it is what stops
	// an unchanged checklist from costing an API call every tick.
	LastText string
}

// GetRunNotification loads the message already published for a run on a
// channel. It returns ErrNotFound when nothing has been published yet.
func (s *Store) GetRunNotification(r RunRef, channel string) (RunNotification, error) {
	var n RunNotification
	err := s.db.QueryRow(`SELECT external_id, last_text FROM run_notifications
		WHERE repo_owner = ? AND repo_name = ? AND issue_number = ? AND channel = ?`,
		r.Owner, r.Name, r.IssueNumber, channel).Scan(&n.ExternalID, &n.LastText)
	if errors.Is(err, sql.ErrNoRows) {
		return RunNotification{}, ErrNotFound
	}
	return n, err
}

// SaveRunNotification records what was published for a run on a channel.
func (s *Store) SaveRunNotification(r RunRef, channel string, n RunNotification) error {
	_, err := s.db.Exec(`
INSERT INTO run_notifications (repo_owner, repo_name, issue_number, channel, external_id, last_text, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repo_owner, repo_name, issue_number, channel)
DO UPDATE SET external_id = excluded.external_id, last_text = excluded.last_text, updated_at = excluded.updated_at`,
		r.Owner, r.Name, r.IssueNumber, channel, n.ExternalID, n.LastText, formatTime(s.Now()))
	return err
}
