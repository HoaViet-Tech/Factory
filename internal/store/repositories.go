package store

import (
	"database/sql"
	"errors"
	"path"
	"strings"

	"github.com/HoaViet-Tech/factory/internal/api"
)

// AddRepository registers a repository, or updates it if it already exists.
func (s *Store) AddRepository(req api.CreateRepositoryRequest) (api.Repository, error) {
	if strings.TrimSpace(req.Owner) == "" || strings.TrimSpace(req.Name) == "" {
		return api.Repository{}, errors.New("owner and name are required")
	}
	cloneURL := strings.TrimSpace(req.CloneURL)
	if cloneURL == "" {
		cloneURL = "https://github.com/" + req.Owner + "/" + req.Name + ".git"
	}
	// The worker passes this straight to `git clone`, and git accepts inputs
	// that are commands rather than addresses. Validate at the point of entry
	// so a bad URL never reaches the database, let alone a shell.
	if err := api.ValidateCloneURL(cloneURL); err != nil {
		return api.Repository{}, err
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// A repository-relative cache path. Each worker resolves it beneath its
	// own --work-dir, so the same row works on several machines.
	cachePath := path.Join("repos", req.Owner, req.Name)
	now := s.Now()

	_, err := s.db.Exec(`
INSERT INTO repositories (owner, name, clone_url, local_cache_path, enabled, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, name) DO UPDATE SET
    clone_url = excluded.clone_url,
    enabled = excluded.enabled`,
		req.Owner, req.Name, cloneURL, cachePath, boolToInt(enabled), formatTime(now))
	if err != nil {
		return api.Repository{}, err
	}
	return s.GetRepository(req.Owner, req.Name)
}

// GetRepository loads one repository by owner and name.
func (s *Store) GetRepository(owner, name string) (api.Repository, error) {
	row := s.db.QueryRow(`SELECT id, owner, name, clone_url, local_cache_path, enabled, created_at
	                      FROM repositories WHERE owner = ? AND name = ?`, owner, name)
	r, err := scanRepository(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Repository{}, ErrNotFound
	}
	return r, err
}

// ListRepositories returns all repositories. When enabledOnly is true, only
// repositories the poller is allowed to touch are returned.
func (s *Store) ListRepositories(enabledOnly bool) ([]api.Repository, error) {
	q := `SELECT id, owner, name, clone_url, local_cache_path, enabled, created_at FROM repositories`
	if enabledOnly {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY owner, name`

	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	repos := []api.Repository{}
	for rows.Next() {
		r, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

func scanRepository(sc interface{ Scan(...any) error }) (api.Repository, error) {
	var (
		r         api.Repository
		enabled   int
		createdAt string
	)
	if err := sc.Scan(&r.ID, &r.Owner, &r.Name, &r.CloneURL, &r.LocalCachePath, &enabled, &createdAt); err != nil {
		return api.Repository{}, err
	}
	r.Enabled = enabled != 0
	r.CreatedAt = mustParseTime(createdAt)
	return r, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
