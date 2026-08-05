package store

import "fmt"

// migrations are applied in order and recorded in schema_migrations. Adding a
// new schema change means appending to this slice and never editing an
// existing entry.
var migrations = []struct {
	Name string
	SQL  string
}{
	{
		Name: "0001_init",
		SQL: `
CREATE TABLE workers (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    runtime      TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    status       TEXT NOT NULL,
    created_at   TEXT NOT NULL
);

CREATE TABLE repositories (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    owner            TEXT NOT NULL,
    name             TEXT NOT NULL,
    clone_url        TEXT NOT NULL,
    local_cache_path TEXT NOT NULL,
    enabled          INTEGER NOT NULL DEFAULT 1,
    created_at       TEXT NOT NULL,
    UNIQUE (owner, name)
);

CREATE TABLE tasks (
    id                  TEXT PRIMARY KEY,
    kind                TEXT NOT NULL,
    status              TEXT NOT NULL,
    repo_owner          TEXT NOT NULL,
    repo_name           TEXT NOT NULL,
    github_issue_number INTEGER,
    title               TEXT NOT NULL,
    prompt              TEXT NOT NULL,
    worker_id           TEXT,
    lease_token         TEXT,
    lease_expires_at    TEXT,
    attempt_count       INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);

CREATE INDEX idx_tasks_status_created ON tasks (status, created_at);

CREATE TABLE task_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    type       TEXT NOT NULL,
    message    TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX idx_task_events_task ON task_events (task_id, id);

CREATE TABLE github_observations (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_owner    TEXT NOT NULL,
    repo_name     TEXT NOT NULL,
    issue_number  INTEGER NOT NULL,
    workflow_kind TEXT NOT NULL,
    dedupe_key    TEXT NOT NULL UNIQUE,
    task_id       TEXT,
    created_at    TEXT NOT NULL
);
`,
	},
	{
		// Workers declare which task kinds they accept, so a pipeline can put a
		// different model on each stage. Empty means "all kinds", which is what
		// every existing row should mean after this migration.
		Name: "0002_worker_kinds",
		SQL: `
ALTER TABLE workers ADD COLUMN kinds TEXT NOT NULL DEFAULT '';
`,
	},
	{
		// Requeued tasks wait before becoming claimable again. Without this a
		// transient failure (a GitHub rate limit, a network blip) burns every
		// attempt within seconds and gives up, when waiting would have worked.
		// Empty means "claimable immediately", which is right for new tasks.
		Name: "0003_task_run_after",
		SQL: `
ALTER TABLE tasks ADD COLUMN run_after TEXT NOT NULL DEFAULT '';
`,
	},
}

// migrate creates schema_migrations if needed and applies anything missing.
func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS schema_migrations (
    name       TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL
);`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, m.Name).Scan(&count); err != nil {
			return fmt.Errorf("check migration %s: %w", m.Name, err)
		}
		if count > 0 {
			continue
		}

		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.Name, err)
		}
		if _, err := tx.Exec(m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.Name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, m.Name, formatTime(s.Now())); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", m.Name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.Name, err)
		}
	}
	return nil
}
