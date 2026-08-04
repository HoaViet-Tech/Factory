// Package worker is the process that actually runs tasks.
//
// The loop is small on purpose: register, heartbeat, claim, execute, complete.
// Everything else (isolation, GitHub side effects, logging) hangs off that
// spine.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HoaViet-Tech/factory/internal/api"
	"github.com/HoaViet-Tech/factory/internal/client"
	"github.com/HoaViet-Tech/factory/internal/githubcli"
	"github.com/HoaViet-Tech/factory/internal/gitx"
	"github.com/HoaViet-Tech/factory/internal/idgen"
	agentruntime "github.com/HoaViet-Tech/factory/internal/runtime"
)

// runtimeResult is a short local alias for the runtime's result type.
type runtimeResult = agentruntime.Result

// Config configures a Worker.
type Config struct {
	ServerURL string
	Name      string
	// ID is a stable identity. When empty it is loaded from (or written to)
	// <WorkDir>/worker-id so restarts keep the same worker row.
	ID string
	// WorkDir holds the repo cache, the worktrees and the worker ID file.
	WorkDir string
	// Runtime executes the task. One worker owns exactly one runtime.
	Runtime agentruntime.Runtime
	// GitHub may be nil to disable all GitHub side effects.
	GitHub *githubcli.Client
	// Push publishes the task branch and opens a draft PR when the agent
	// produced changes. Off by default: pushing is a visible, outward action.
	Push bool
	// PollInterval is how often an idle worker asks for work.
	PollInterval time.Duration
	// HeartbeatInterval is how often the worker reports it is alive.
	HeartbeatInterval time.Duration
	// LeaseSeconds is how long a claimed task stays leased.
	LeaseSeconds int
	// TaskTimeout bounds one runtime execution.
	TaskTimeout time.Duration
	// Once makes the worker exit after finishing one task, which is handy for
	// scripted demos and tests.
	Once   bool
	Logger *log.Logger
}

// Worker is one worker process.
type Worker struct {
	cfg    Config
	api    *client.Client
	logger *log.Logger
	id     string
}

// New builds a worker, resolving its stable identity.
func New(cfg Config) (*Worker, error) {
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stdout, "[worker] ", log.LstdFlags)
	}
	if cfg.Runtime == nil {
		return nil, errors.New("worker requires a runtime")
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = ".factory"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 20 * time.Second
	}
	if cfg.LeaseSeconds <= 0 {
		cfg.LeaseSeconds = 120
	}
	if cfg.TaskTimeout <= 0 {
		cfg.TaskTimeout = 30 * time.Minute
	}

	abs, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	cfg.WorkDir = abs
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}

	id, err := resolveWorkerID(cfg)
	if err != nil {
		return nil, err
	}

	return &Worker{
		cfg:    cfg,
		api:    client.New(cfg.ServerURL),
		logger: cfg.Logger,
		id:     id,
	}, nil
}

// resolveWorkerID keeps a worker's identity stable across restarts by caching
// it in a file. Without this, every restart would leak a new worker row.
func resolveWorkerID(cfg Config) (string, error) {
	if cfg.ID != "" {
		return cfg.ID, nil
	}
	path := filepath.Join(cfg.WorkDir, "worker-id")
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}
	id := idgen.WorkerID()
	if err := os.WriteFile(path, []byte(id+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("persist worker id: %w", err)
	}
	return id, nil
}

// ID returns this worker's stable identity.
func (w *Worker) ID() string { return w.id }

// Run registers the worker and then loops until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	worker, err := w.api.RegisterWorker(api.RegisterWorkerRequest{
		ID:      w.id,
		Name:    w.cfg.Name,
		Runtime: w.cfg.Runtime.Name(),
	})
	if err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	w.logger.Printf("registered as %s (name=%s runtime=%s)", worker.ID, worker.Name, worker.Runtime)
	w.logger.Printf("work dir: %s", w.cfg.WorkDir)

	go w.heartbeatLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			w.logger.Printf("shutting down")
			return nil
		default:
		}

		task, lease, err := w.api.Claim(w.id, w.cfg.LeaseSeconds)
		if errors.Is(err, client.ErrNoTask) {
			if w.cfg.Once {
				w.logger.Printf("queue empty and --once set; exiting")
				return nil
			}
			if !sleepCtx(ctx, w.cfg.PollInterval) {
				return nil
			}
			continue
		}
		if err != nil {
			w.logger.Printf("claim failed: %v", err)
			if !sleepCtx(ctx, w.cfg.PollInterval) {
				return nil
			}
			continue
		}

		w.logger.Printf("claimed task %s (%s) %q", task.ID, task.Kind, task.Title)
		w.runTask(ctx, task, lease)

		if w.cfg.Once {
			w.logger.Printf("--once set; exiting after one task")
			return nil
		}
	}
}

// heartbeatLoop keeps the worker's lease-owning identity alive.
func (w *Worker) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(w.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.api.Heartbeat(w.id); err != nil {
				w.logger.Printf("heartbeat failed: %v", err)
			}
		}
	}
}

// runTask executes one claimed task and always completes it, so a lease is
// never left dangling by a code path that forgot to report back.
func (w *Worker) runTask(ctx context.Context, task api.Task, lease string) {
	logf := w.taskLogger(task.ID, lease)

	taskCtx, cancel := context.WithTimeout(ctx, w.cfg.TaskTimeout)
	defer cancel()

	// A lease renewal loop would be the next step for very long tasks; for the
	// MVP the lease is simply set long enough, and an over-run is reaped and
	// retried.
	status, summary := api.StatusSucceeded, ""
	result, err := w.execute(taskCtx, task, logf)
	if err != nil {
		status = api.StatusFailed
		summary = err.Error()
		logf(api.EventError, "task failed: %v", err)
	} else {
		summary = result.Summary
	}

	if _, err := w.api.Complete(task.ID, lease, status, summary); err != nil {
		w.logger.Printf("could not complete task %s: %v", task.ID, err)
		return
	}
	w.logger.Printf("task %s -> %s", task.ID, status)
}

// taskLogger returns a logger that writes to both the local console and the
// task's event log on the server.
func (w *Worker) taskLogger(taskID, lease string) func(evType, format string, args ...any) {
	return func(evType, format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		w.logger.Printf("[%s] %s", taskID, msg)
		if err := w.api.AppendEvent(taskID, lease, evType, msg); err != nil {
			w.logger.Printf("[%s] could not record event: %v", taskID, err)
		}
	}
}

// execute performs the full per-task pipeline described in the README:
// cache -> branch -> worktree -> prompt file -> runtime -> git status ->
// GitHub side effects.
func (w *Worker) execute(ctx context.Context, task api.Task, logf func(string, string, ...any)) (agentruntime.Result, error) {
	repo, err := w.repoForTask(task)
	if err != nil {
		return agentruntime.Result{}, err
	}

	// GitHub-triggered work revalidates the issue's labels *now*, because the
	// snapshot taken at poll time may be minutes old and a human may have
	// changed their mind since.
	var issue *githubcli.Issue
	if task.GitHubIssueNumber != nil && w.cfg.GitHub != nil {
		iss, skip, err := w.revalidateIssue(task, logf)
		if err != nil {
			return agentruntime.Result{}, err
		}
		if skip != "" {
			return agentruntime.Result{Summary: skip}, nil
		}
		issue = iss
	}

	gitLog := func(format string, args ...any) { logf(api.EventLog, format, args...) }

	cacheDir := filepath.Join(w.cfg.WorkDir, filepath.FromSlash(repo.LocalCachePath))
	logf(api.EventInfo, "preparing repo cache at %s", cacheDir)
	cache, err := gitx.EnsureCache(cacheDir, repo.CloneURL, gitLog)
	if err != nil {
		return agentruntime.Result{}, err
	}

	defaultBranch, err := cache.DefaultBranch()
	if err != nil {
		return agentruntime.Result{}, fmt.Errorf("determine default branch: %w", err)
	}
	baseRef := cache.BaseRef(defaultBranch)

	branch := "factory/task-" + task.ID
	worktreeDir := filepath.Join(w.cfg.WorkDir, "worktrees", task.ID)
	logf(api.EventInfo, "creating worktree %s on branch %s (from %s)", worktreeDir, branch, baseRef)

	wt, err := cache.AddWorktree(worktreeDir, branch, baseRef)
	if err != nil {
		return agentruntime.Result{}, fmt.Errorf("create worktree: %w", err)
	}

	// The prompt goes in the worktree so the agent can read it as a file, and
	// so it is visible in the diff-free workspace while debugging.
	promptFile := filepath.Join(worktreeDir, ".factory-task.md")
	if err := os.WriteFile(promptFile, []byte(task.Prompt), 0o644); err != nil {
		return agentruntime.Result{}, fmt.Errorf("write prompt file: %w", err)
	}
	logf(api.EventInfo, "wrote prompt to .factory-task.md (%d bytes)", len(task.Prompt))

	result, err := w.cfg.Runtime.Run(agentruntime.RunContext{
		Ctx:         ctx,
		Task:        task,
		WorktreeDir: worktreeDir,
		PromptFile:  promptFile,
		Prompt:      task.Prompt,
		Log:         func(format string, args ...any) { logf(api.EventLog, format, args...) },
	})
	if err != nil {
		return agentruntime.Result{}, err
	}

	status, err := wt.Status()
	if err != nil {
		return result, fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		logf(api.EventInfo, "git status: worktree is clean (no changes)")
	} else {
		logf(api.EventInfo, "git status:\n%s", status)
	}

	// GitHub side effects come last, once the work is known to have succeeded.
	if issue != nil {
		switch task.Kind {
		case api.KindRefineTicket:
			if err := w.publishRefinement(task, *issue, result, logf); err != nil {
				return result, err
			}
		case api.KindImplementTicket:
			if err := w.publishImplementation(task, *issue, wt, baseRef, result, logf); err != nil {
				return result, err
			}
		}
	}

	if result.Summary == "" {
		result.Summary = "task completed"
	}
	logf(api.EventInfo, "worktree left at %s for inspection", worktreeDir)
	return result, nil
}

// repoForTask finds the registered repository for a task.
func (w *Worker) repoForTask(task api.Task) (api.Repository, error) {
	repos, err := w.api.ListRepositories()
	if err != nil {
		return api.Repository{}, fmt.Errorf("list repositories: %w", err)
	}
	for _, r := range repos {
		if r.Owner == task.RepoOwner && r.Name == task.RepoName {
			return r, nil
		}
	}
	return api.Repository{}, fmt.Errorf("repository %s is not registered; add it with `codefactory repo add %s`",
		task.FullName(), task.FullName())
}

// sleepCtx sleeps unless ctx is cancelled first. It reports false when the
// context ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
