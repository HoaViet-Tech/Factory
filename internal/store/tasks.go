package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/idgen"
)

// MaxAttempts is how many times a task may be claimed before an expired lease
// marks it "lost" instead of putting it back on the queue.
const MaxAttempts = 3

const taskColumns = `id, kind, status, repo_owner, repo_name, github_issue_number,
	title, prompt, worker_id, lease_token, lease_expires_at, attempt_count,
	created_at, updated_at`

// scanTask reads one task row. Works with both *sql.Row and *sql.Rows.
func scanTask(sc interface{ Scan(...any) error }) (api.Task, error) {
	var (
		t         api.Task
		issueNum  sql.NullInt64
		workerID  sql.NullString
		leaseTok  sql.NullString
		leaseExp  sql.NullString
		createdAt string
		updatedAt string
	)
	err := sc.Scan(
		&t.ID, &t.Kind, &t.Status, &t.RepoOwner, &t.RepoName, &issueNum,
		&t.Title, &t.Prompt, &workerID, &leaseTok, &leaseExp, &t.AttemptCount,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return api.Task{}, err
	}
	t.GitHubIssueNumber = nullInt(issueNum)
	t.WorkerID = nullString(workerID)
	t.LeaseToken = nullString(leaseTok)
	t.LeaseExpiresAt = nullTime(leaseExp)
	t.CreatedAt = mustParseTime(createdAt)
	t.UpdatedAt = mustParseTime(updatedAt)
	return t, nil
}

// CreateTask inserts a queued task and its "created" event.
func (s *Store) CreateTask(req api.CreateTaskRequest) (api.Task, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return api.Task{}, err
	}
	defer tx.Rollback()

	t, err := s.createTaskTx(tx, req)
	if err != nil {
		return api.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Task{}, err
	}
	return t, nil
}

// createTaskTx is the shared insert used by CreateTask and by the GitHub
// dedupe path, which needs the insert to happen inside its own transaction.
func (s *Store) createTaskTx(tx *sql.Tx, req api.CreateTaskRequest) (api.Task, error) {
	if req.Kind == "" {
		req.Kind = api.KindManual
	}
	if !api.IsValidKind(req.Kind) {
		return api.Task{}, fmt.Errorf("invalid task kind %q (valid: %s)", req.Kind, strings.Join(api.ValidKinds(), ", "))
	}
	if strings.TrimSpace(req.Title) == "" {
		return api.Task{}, errors.New("title is required")
	}
	if strings.TrimSpace(req.RepoOwner) == "" || strings.TrimSpace(req.RepoName) == "" {
		return api.Task{}, errors.New("repo_owner and repo_name are required")
	}

	now := s.Now()
	t := api.Task{
		ID:                idgen.TaskID(),
		Kind:              req.Kind,
		Status:            api.StatusQueued,
		RepoOwner:         req.RepoOwner,
		RepoName:          req.RepoName,
		GitHubIssueNumber: req.GitHubIssueNumber,
		Title:             req.Title,
		Prompt:            req.Prompt,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	_, err := tx.Exec(`
INSERT INTO tasks (`+taskColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, 0, ?, ?)`,
		t.ID, t.Kind, t.Status, t.RepoOwner, t.RepoName, intOrNil(t.GitHubIssueNumber),
		t.Title, t.Prompt, formatTime(t.CreatedAt), formatTime(t.UpdatedAt))
	if err != nil {
		return api.Task{}, fmt.Errorf("insert task: %w", err)
	}

	if err := appendEventTx(tx, s.Now(), t.ID, api.EventCreated,
		fmt.Sprintf("task created (kind=%s repo=%s/%s)", t.Kind, t.RepoOwner, t.RepoName)); err != nil {
		return api.Task{}, err
	}
	return t, nil
}

// GetTask loads one task by ID.
func (s *Store) GetTask(id string) (api.Task, error) {
	row := s.db.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Task{}, ErrNotFound
	}
	return t, err
}

// TaskFilter narrows ListTasks.
type TaskFilter struct {
	Status string
	Kind   string
	Repo   string // "owner/name"
	Limit  int
}

// ListTasks returns tasks newest first.
func (s *Store) ListTasks(f TaskFilter) ([]api.Task, error) {
	q := `SELECT ` + taskColumns + ` FROM tasks WHERE 1=1`
	var args []any

	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if f.Repo != "" {
		owner, name, ok := strings.Cut(f.Repo, "/")
		if !ok {
			return nil, fmt.Errorf("repo filter %q must be owner/name", f.Repo)
		}
		q += ` AND repo_owner = ? AND repo_name = ?`
		args = append(args, owner, name)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := s.db.Query(q, args...)
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

// ClaimTask atomically hands the oldest queued task to a worker and stamps a
// lease on it. It returns ErrNotFound when the queue is empty.
//
// The lease is the whole trick that makes dead workers safe: if the worker
// disappears, the lease expires and ReapExpiredLeases puts the task back.
func (s *Store) ClaimTask(workerID string, leaseFor time.Duration) (api.Task, string, error) {
	if workerID == "" {
		return api.Task{}, "", errors.New("worker_id is required")
	}
	if leaseFor <= 0 {
		leaseFor = 2 * time.Minute
	}

	tx, err := s.db.Begin()
	if err != nil {
		return api.Task{}, "", err
	}
	defer tx.Rollback()

	// Oldest queued task first: a plain FIFO queue.
	row := tx.QueryRow(`SELECT ` + taskColumns + ` FROM tasks
		WHERE status = '` + api.StatusQueued + `'
		ORDER BY created_at ASC, id ASC LIMIT 1`)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Task{}, "", ErrNotFound
	}
	if err != nil {
		return api.Task{}, "", err
	}

	now := s.Now()
	token := idgen.LeaseToken()
	expires := now.Add(leaseFor)

	res, err := tx.Exec(`UPDATE tasks
		SET status = ?, worker_id = ?, lease_token = ?, lease_expires_at = ?,
		    attempt_count = attempt_count + 1, updated_at = ?
		WHERE id = ? AND status = ?`,
		api.StatusRunning, workerID, token, formatTime(expires), formatTime(now),
		t.ID, api.StatusQueued)
	if err != nil {
		return api.Task{}, "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return api.Task{}, "", err
	}
	if n == 0 {
		// Someone else claimed it between the SELECT and the UPDATE.
		return api.Task{}, "", ErrNotFound
	}

	if err := appendEventTx(tx, now, t.ID, api.EventClaimed,
		fmt.Sprintf("claimed by worker %s (attempt %d, lease expires %s)",
			workerID, t.AttemptCount+1, expires.Format(time.RFC3339))); err != nil {
		return api.Task{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return api.Task{}, "", err
	}

	t.Status = api.StatusRunning
	t.WorkerID = &workerID
	t.LeaseToken = &token
	t.LeaseExpiresAt = &expires
	t.AttemptCount++
	t.UpdatedAt = now
	return t, token, nil
}

// checkLease loads a running task and verifies the presented lease token.
func checkLease(tx *sql.Tx, id, token string) (api.Task, error) {
	row := tx.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Task{}, ErrNotFound
	}
	if err != nil {
		return api.Task{}, err
	}
	if t.Status != api.StatusRunning {
		return t, ErrNotRunning
	}
	if t.LeaseToken == nil || *t.LeaseToken != token || token == "" {
		return t, ErrInvalidLease
	}
	return t, nil
}

// RenewLease extends a running task's lease.
//
// This is what makes long agent runs safe. A worker renews on a timer while it
// works; if it dies, renewals stop, the lease expires, and the reaper requeues
// the task. Without renewal the reaper cannot tell "still working" from
// "dead", and a slow task gets executed twice concurrently.
func (s *Store) RenewLease(id, leaseToken string, extendBy time.Duration) (api.Task, error) {
	if extendBy <= 0 {
		extendBy = 2 * time.Minute
	}

	tx, err := s.db.Begin()
	if err != nil {
		return api.Task{}, err
	}
	defer tx.Rollback()

	// checkLease enforces both "is running" and "owns the lease", so a worker
	// whose lease was already reaped cannot resurrect it.
	if _, err := checkLease(tx, id, leaseToken); err != nil {
		return api.Task{}, err
	}

	now := s.Now()
	expires := now.Add(extendBy)
	if _, err := tx.Exec(`UPDATE tasks SET lease_expires_at = ?, updated_at = ? WHERE id = ?`,
		formatTime(expires), formatTime(now), id); err != nil {
		return api.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Task{}, err
	}
	return s.GetTask(id)
}

// CompleteTask moves a running task into a terminal state. The caller must
// present the lease token it was given at claim time.
func (s *Store) CompleteTask(id, leaseToken, status, message string) (api.Task, error) {
	if !api.IsTerminal(status) {
		return api.Task{}, fmt.Errorf("status %q is not a terminal status", status)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return api.Task{}, err
	}
	defer tx.Rollback()

	if _, err := checkLease(tx, id, leaseToken); err != nil {
		return api.Task{}, err
	}

	now := s.Now()
	if _, err := tx.Exec(`UPDATE tasks
		SET status = ?, lease_token = NULL, lease_expires_at = NULL, updated_at = ?
		WHERE id = ?`, status, formatTime(now), id); err != nil {
		return api.Task{}, err
	}

	msg := message
	if msg == "" {
		msg = "task finished with status " + status
	}
	if err := appendEventTx(tx, now, id, api.EventComplete, msg); err != nil {
		return api.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Task{}, err
	}
	return s.GetTask(id)
}

// AppendTaskEventWithLease records a log line from a worker. Running tasks
// require a valid lease; this stops a stale worker from writing into a task it
// no longer owns.
func (s *Store) AppendTaskEventWithLease(id, leaseToken, evType, message string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := checkLease(tx, id, leaseToken); err != nil {
		return err
	}
	if err := appendEventTx(tx, s.Now(), id, evType, message); err != nil {
		return err
	}
	return tx.Commit()
}

// CancelTask cancels a queued or running task.
func (s *Store) CancelTask(id string) (api.Task, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return api.Task{}, err
	}
	defer tx.Rollback()

	row := tx.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Task{}, ErrNotFound
	}
	if err != nil {
		return api.Task{}, err
	}
	if api.IsTerminal(t.Status) {
		return t, ErrTerminal
	}

	now := s.Now()
	if _, err := tx.Exec(`UPDATE tasks
		SET status = ?, lease_token = NULL, lease_expires_at = NULL, updated_at = ?
		WHERE id = ?`, api.StatusCancelled, formatTime(now), id); err != nil {
		return api.Task{}, err
	}
	if err := appendEventTx(tx, now, id, api.EventInfo, "task cancelled"); err != nil {
		return api.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Task{}, err
	}
	return s.GetTask(id)
}

// ReapedTask describes one lease the reaper expired.
type ReapedTask struct {
	TaskID    string
	NewStatus string
}

// ReapExpiredLeases finds running tasks whose lease has expired and either
// requeues them (so another worker can retry) or marks them lost once they
// have burned through MaxAttempts.
func (s *Store) ReapExpiredLeases() ([]ReapedTask, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := s.Now()
	rows, err := tx.Query(`SELECT `+taskColumns+` FROM tasks
		WHERE status = ? AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
		api.StatusRunning, formatTime(now))
	if err != nil {
		return nil, err
	}

	var expired []api.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		expired = append(expired, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var reaped []ReapedTask
	for _, t := range expired {
		newStatus := api.StatusQueued
		msg := fmt.Sprintf("lease expired after attempt %d; requeued", t.AttemptCount)
		if t.AttemptCount >= MaxAttempts {
			newStatus = api.StatusLost
			msg = fmt.Sprintf("lease expired after attempt %d; giving up (max %d attempts)", t.AttemptCount, MaxAttempts)
		}

		if _, err := tx.Exec(`UPDATE tasks
			SET status = ?, worker_id = NULL, lease_token = NULL,
			    lease_expires_at = NULL, updated_at = ?
			WHERE id = ?`, newStatus, formatTime(now), t.ID); err != nil {
			return nil, err
		}
		if err := appendEventTx(tx, now, t.ID, api.EventWarn, msg); err != nil {
			return nil, err
		}
		reaped = append(reaped, ReapedTask{TaskID: t.ID, NewStatus: newStatus})
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return reaped, nil
}
