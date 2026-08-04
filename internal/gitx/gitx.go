// Package gitx wraps the git commands the worker needs.
//
// Isolation is the point of this package: every task gets its own branch in
// its own worktree, so two agents can work on the same repository at the same
// time without ever seeing each other's files.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Repo is a local clone that worktrees are created from.
type Repo struct {
	// Dir is the cache clone directory.
	Dir string
	// Logf receives every command that runs. May be nil.
	Logf func(format string, args ...any)
}

func (r *Repo) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Run executes a git command in dir and returns trimmed stdout.
func Run(dir string, logf func(string, ...any), args ...string) (string, error) {
	return RunWithTimeout(dir, 5*time.Minute, logf, args...)
}

// RunWithTimeout executes a git command with an explicit timeout.
func RunWithTimeout(dir string, timeout time.Duration, logf func(string, ...any), args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if logf != nil {
		logf("git %s", strings.Join(args, " "))
	}
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

func (r *Repo) git(args ...string) (string, error) {
	return Run(r.Dir, r.Logf, args...)
}

// EnsureCache clones cloneURL into dir if it is not there yet, and otherwise
// fetches the latest refs.
//
// cloneURL may be a local path, which is what makes the credential-free demo
// possible.
func EnsureCache(dir, cloneURL string, logf func(string, ...any)) (*Repo, error) {
	repo := &Repo{Dir: dir, Logf: logf}

	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		// Already cloned: refresh. A fetch failure is not fatal (you may be
		// offline, or the source may be a local path that moved), so it is
		// logged and the stale cache is used.
		if _, err := repo.git("fetch", "--prune", "origin"); err != nil {
			if logf != nil {
				logf("warning: fetch failed, continuing with cached clone: %v", err)
			}
		}
		return repo, nil
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, fmt.Errorf("create cache parent: %w", err)
	}
	if _, err := Run("", logf, "clone", cloneURL, dir); err != nil {
		return nil, fmt.Errorf("clone %s: %w", cloneURL, err)
	}
	return repo, nil
}

// DefaultBranch reports the cache clone's default branch.
func (r *Repo) DefaultBranch() (string, error) {
	// The remote's HEAD is the most reliable answer when it is present.
	if out, err := r.git("symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && out != "" {
		if _, name, ok := strings.Cut(out, "/"); ok && name != "" {
			return name, nil
		}
	}
	out, err := r.git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if out == "" || out == "HEAD" {
		return "main", nil
	}
	return out, nil
}

// BaseRef returns the ref a new task branch should start from, preferring the
// remote-tracking branch so that work always starts from the latest fetch.
func (r *Repo) BaseRef(branch string) string {
	if _, err := r.git("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		return "origin/" + branch
	}
	return branch
}

// Worktree is one isolated checkout dedicated to a single task.
type Worktree struct {
	Dir    string
	Branch string
	repo   *Repo
}

// AddWorktree creates a new branch and checks it out into its own directory.
func (r *Repo) AddWorktree(dir, branch, baseRef string) (*Worktree, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, fmt.Errorf("create worktree parent: %w", err)
	}
	// A leftover directory from a previous attempt would make `git worktree
	// add` fail with a confusing message.
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("worktree directory %s already exists (remove it, or run `git worktree prune`)", dir)
	}
	if _, err := r.git("worktree", "add", "-b", branch, dir, baseRef); err != nil {
		return nil, err
	}
	return &Worktree{Dir: dir, Branch: branch, repo: r}, nil
}

func (w *Worktree) git(args ...string) (string, error) {
	return Run(w.Dir, w.repo.Logf, args...)
}

// Status returns `git status --porcelain` for the worktree, ignoring the
// factory's own scratch files.
//
// Excluding them here matters: otherwise the prompt file alone would make
// every task look like it produced changes.
func (w *Worktree) Status() (string, error) {
	args := []string{"status", "--porcelain", "--", "."}
	for _, f := range FactoryFiles {
		args = append(args, ":(exclude)"+f)
	}
	return w.git(args...)
}

// HasChanges reports whether the agent actually changed anything.
func (w *Worktree) HasChanges() (bool, error) {
	out, err := w.Status()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// FactoryFiles are the scratch files the factory itself writes into a
// worktree. They are excluded from commits so a review never has to wade
// through the machinery's own bookkeeping.
var FactoryFiles = []string{".factory-task.md", ".factory-refined.md"}

// CommitAll stages the agent's work and commits it, leaving the factory's own
// scratch files out.
//
// The identity is passed with -c so the factory never mutates the user's git
// config.
func (w *Worktree) CommitAll(message, authorName, authorEmail string) error {
	add := []string{"add", "-A", "--", "."}
	for _, f := range FactoryFiles {
		add = append(add, ":(exclude)"+f)
	}
	if _, err := w.git(add...); err != nil {
		return err
	}
	_, err := w.git(
		"-c", "user.name="+authorName,
		"-c", "user.email="+authorEmail,
		"commit", "-m", message,
	)
	return err
}

// Push publishes the task branch to origin.
func (w *Worktree) Push() error {
	_, err := w.git("push", "--set-upstream", "origin", w.Branch)
	return err
}

// Diffstat summarises the change for a task log or PR body.
func (w *Worktree) Diffstat(baseRef string) (string, error) {
	return w.git("diff", "--stat", baseRef+"...HEAD")
}

// ChangedFiles lists files changed relative to baseRef.
func (w *Worktree) ChangedFiles(baseRef string) ([]string, error) {
	out, err := w.git("diff", "--name-only", baseRef+"...HEAD")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n"), nil
}
