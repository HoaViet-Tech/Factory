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

// StaleAfter is how long a worker may go without a heartbeat before it is
// reported as stale.
const StaleAfter = 90 * time.Second

// RegisterWorker upserts a worker identity.
//
// A worker passes a stable ID so that restarting the process keeps the same
// identity instead of leaving orphan rows behind. One worker ID owns exactly
// one runtime; re-registering with a different runtime updates it.
func (s *Store) RegisterWorker(req api.RegisterWorkerRequest) (api.Worker, error) {
	if strings.TrimSpace(req.Name) == "" {
		return api.Worker{}, errors.New("name is required")
	}
	if strings.TrimSpace(req.Runtime) == "" {
		return api.Worker{}, errors.New("runtime is required")
	}

	for _, k := range req.Kinds {
		if !api.IsValidKind(k) {
			return api.Worker{}, fmt.Errorf("invalid task kind %q (valid: %s)",
				k, strings.Join(api.ValidKinds(), ", "))
		}
	}

	id := req.ID
	if id == "" {
		id = idgen.WorkerID()
	}
	now := s.Now()

	_, err := s.db.Exec(`
INSERT INTO workers (id, name, runtime, kinds, last_seen_at, status, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    name = excluded.name,
    runtime = excluded.runtime,
    kinds = excluded.kinds,
    last_seen_at = excluded.last_seen_at,
    status = excluded.status`,
		id, req.Name, req.Runtime, encodeKinds(req.Kinds), formatTime(now), api.WorkerActive, formatTime(now))
	if err != nil {
		return api.Worker{}, err
	}
	return s.GetWorker(id)
}

// Heartbeat refreshes a worker's last_seen_at.
func (s *Store) Heartbeat(workerID string) (api.Worker, error) {
	now := s.Now()
	res, err := s.db.Exec(`UPDATE workers SET last_seen_at = ?, status = ? WHERE id = ?`,
		formatTime(now), api.WorkerActive, workerID)
	if err != nil {
		return api.Worker{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return api.Worker{}, err
	}
	if n == 0 {
		return api.Worker{}, ErrNotFound
	}
	return s.GetWorker(workerID)
}

// GetWorker loads a worker by ID.
func (s *Store) GetWorker(id string) (api.Worker, error) {
	row := s.db.QueryRow(`SELECT id, name, runtime, kinds, last_seen_at, status, created_at
	                      FROM workers WHERE id = ?`, id)
	w, err := s.scanWorker(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Worker{}, ErrNotFound
	}
	return w, err
}

// ListWorkers returns every registered worker, most recently seen first.
func (s *Store) ListWorkers() ([]api.Worker, error) {
	rows, err := s.db.Query(`SELECT id, name, runtime, kinds, last_seen_at, status, created_at
	                         FROM workers ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	workers := []api.Worker{}
	for rows.Next() {
		w, err := s.scanWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}

// encodeKinds stores the kind list as a comma-separated string. A dedicated
// table would be tidier, but this stays readable in a SQLite browser and the
// list has at most four entries.
func encodeKinds(kinds []string) string { return strings.Join(kinds, ",") }

func decodeKinds(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Store) scanWorker(sc interface{ Scan(...any) error }) (api.Worker, error) {
	var (
		w          api.Worker
		kinds      string
		lastSeenAt string
		createdAt  string
	)
	if err := sc.Scan(&w.ID, &w.Name, &w.Runtime, &kinds, &lastSeenAt, &w.Status, &createdAt); err != nil {
		return api.Worker{}, err
	}
	w.Kinds = decodeKinds(kinds)
	w.LastSeenAt = mustParseTime(lastSeenAt)
	w.CreatedAt = mustParseTime(createdAt)

	// Staleness is derived at read time rather than written by a background
	// job: one less moving part.
	if s.Now().Sub(w.LastSeenAt) > StaleAfter {
		w.Status = api.WorkerStale
	}
	return w, nil
}
