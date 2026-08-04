// Package store is the SQLite persistence layer for the control plane.
//
// Everything the control plane knows lives here: repositories, workers, the
// task queue, task events, and the GitHub observations used for deduplication.
// The SQL is deliberately plain and explicit so it can be read top to bottom.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so `go build` works everywhere
)

// Sentinel errors. Callers (the HTTP layer especially) map these onto status
// codes, so they are part of the package's contract.
var (
	ErrNotFound     = errors.New("not found")
	ErrInvalidLease = errors.New("invalid lease token")
	ErrNotRunning   = errors.New("task is not running")
	ErrTerminal     = errors.New("task is already in a terminal state")
)

// Store owns the database handle.
type Store struct {
	db *sql.DB
	// now is overridable in tests so lease expiry can be exercised without
	// sleeping.
	now func() time.Time
}

// Open opens (and creates if necessary) the SQLite database at path and
// applies the schema.
//
// path may also be ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := buildDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	// One connection keeps writes serialised. This project is a single-node
	// local control plane, so the simplicity is worth more than the
	// concurrency, and it removes a whole class of "database is locked" bugs.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}

	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// buildDSN adds the pragmas we always want. Foreign keys are off by default in
// SQLite, and a busy timeout avoids spurious lock errors.
func buildDSN(path string) string {
	if path == ":memory:" || path == "" {
		return ":memory:?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	return "file:" + path + "?" + q.Encode()
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle. Tests use it; production code should not.
func (s *Store) DB() *sql.DB { return s.db }

// SetClock replaces the store's clock. Tests use this to simulate the passage
// of time (for example, to expire a lease without waiting).
func (s *Store) SetClock(fn func() time.Time) { s.now = fn }

// Now returns the store's current time in UTC.
func (s *Store) Now() time.Time { return s.now().UTC() }

// ---------- time helpers ----------
//
// Times are stored as RFC3339Nano UTC strings. That keeps the database
// human-readable, which matters a lot when you are learning and want to poke
// at it with the sqlite3 CLI.

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func mustParseTime(s string) time.Time {
	t, err := parseTime(s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// nullTime converts a nullable TEXT column into *time.Time.
func nullTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := mustParseTime(ns.String)
	return &t
}

// nullString converts a nullable TEXT column into *string.
func nullString(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

// nullInt converts a nullable INTEGER column into *int.
func nullInt(ni sql.NullInt64) *int {
	if !ni.Valid {
		return nil
	}
	v := int(ni.Int64)
	return &v
}

// strOrNil turns an optional string into a driver argument.
func strOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// intOrNil turns an optional int into a driver argument.
func intOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// timeOrNil turns an optional time into a driver argument.
func timeOrNil(p *time.Time) any {
	if p == nil {
		return nil
	}
	return formatTime(*p)
}
