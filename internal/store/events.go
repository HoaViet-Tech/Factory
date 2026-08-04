package store

import (
	"database/sql"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
)

// appendEventTx inserts one task event inside an existing transaction.
func appendEventTx(tx *sql.Tx, now time.Time, taskID, evType, message string) error {
	if evType == "" {
		evType = api.EventInfo
	}
	_, err := tx.Exec(`INSERT INTO task_events (task_id, type, message, created_at) VALUES (?, ?, ?, ?)`,
		taskID, evType, message, formatTime(now))
	return err
}

// AppendEvent records an event without requiring a lease. The server uses this
// for its own bookkeeping (GitHub actions, reaping); worker log lines go
// through AppendTaskEventWithLease instead.
func (s *Store) AppendEvent(taskID, evType, message string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := appendEventTx(tx, s.Now(), taskID, evType, message); err != nil {
		return err
	}
	return tx.Commit()
}

// ListTaskEvents returns a task's events oldest first, which is the order you
// want when reading them as a log.
func (s *Store) ListTaskEvents(taskID string, limit int) ([]api.TaskEvent, error) {
	// Surface a clear 404 rather than an empty list for an unknown task.
	if _, err := s.GetTask(taskID); err != nil {
		return nil, err
	}

	q := `SELECT id, task_id, type, message, created_at FROM task_events
	      WHERE task_id = ? ORDER BY id ASC`
	args := []any{taskID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := []api.TaskEvent{}
	for rows.Next() {
		var (
			e         api.TaskEvent
			createdAt string
		)
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Type, &e.Message, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt = mustParseTime(createdAt)
		events = append(events, e)
	}
	return events, rows.Err()
}
