package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/HoaViet-Tech/factory/internal/api"
)

// DedupeKey is the identity of "we already reacted to this issue for this
// workflow". Polling is at-least-once, so every GitHub-triggered task must be
// keyed by something stable.
func DedupeKey(owner, name string, issueNumber int, workflowKind string) string {
	return fmt.Sprintf("%s/%s#%d:%s", owner, name, issueNumber, workflowKind)
}

// CreateTaskForIssue creates a task for a GitHub issue exactly once per dedupe
// key.
//
// The observation insert and the task insert happen in one transaction, and
// the UNIQUE constraint on dedupe_key is what enforces the guarantee. Polling
// the same issue a hundred times therefore produces exactly one task.
//
// It returns created=false and the existing task when the key was already seen.
func (s *Store) CreateTaskForIssue(dedupeKey string, req api.CreateTaskRequest) (task api.Task, created bool, err error) {
	if dedupeKey == "" {
		return api.Task{}, false, errors.New("dedupe key is required")
	}
	if req.GitHubIssueNumber == nil {
		return api.Task{}, false, errors.New("github_issue_number is required for issue-triggered tasks")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return api.Task{}, false, err
	}
	defer tx.Rollback()

	// Has this issue already produced a task for this workflow?
	var existingTaskID sql.NullString
	err = tx.QueryRow(`SELECT task_id FROM github_observations WHERE dedupe_key = ?`, dedupeKey).
		Scan(&existingTaskID)
	switch {
	case err == nil:
		// Already observed. Return the task it produced, if it still exists.
		if existingTaskID.Valid {
			row := tx.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, existingTaskID.String)
			t, terr := scanTask(row)
			if terr == nil {
				return t, false, nil
			}
			if !errors.Is(terr, sql.ErrNoRows) {
				return api.Task{}, false, terr
			}
		}
		return api.Task{}, false, nil
	case errors.Is(err, sql.ErrNoRows):
		// Not seen before: fall through and create.
	default:
		return api.Task{}, false, err
	}

	t, err := s.createTaskTx(tx, req)
	if err != nil {
		return api.Task{}, false, err
	}

	_, err = tx.Exec(`
INSERT INTO github_observations (repo_owner, repo_name, issue_number, workflow_kind, dedupe_key, task_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		req.RepoOwner, req.RepoName, *req.GitHubIssueNumber, req.Kind, dedupeKey, t.ID, formatTime(s.Now()))
	if err != nil {
		// A concurrent poller won the race; the UNIQUE constraint fired and
		// this transaction rolls back, so no duplicate task survives.
		return api.Task{}, false, fmt.Errorf("record github observation: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return api.Task{}, false, err
	}
	return t, true, nil
}

// ListObservations returns recorded GitHub observations, newest first.
func (s *Store) ListObservations() ([]api.GitHubObservation, error) {
	rows, err := s.db.Query(`SELECT id, repo_owner, repo_name, issue_number, workflow_kind,
	                                dedupe_key, task_id, created_at
	                         FROM github_observations ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []api.GitHubObservation{}
	for rows.Next() {
		var (
			o         api.GitHubObservation
			taskID    sql.NullString
			createdAt string
		)
		if err := rows.Scan(&o.ID, &o.RepoOwner, &o.RepoName, &o.IssueNumber,
			&o.WorkflowKind, &o.DedupeKey, &taskID, &createdAt); err != nil {
			return nil, err
		}
		o.TaskID = nullString(taskID)
		o.CreatedAt = mustParseTime(createdAt)
		out = append(out, o)
	}
	return out, rows.Err()
}
