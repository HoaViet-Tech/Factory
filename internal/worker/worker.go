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
	"github.com/HoaViet-Tech/factory/internal/prompt"
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
	// Kinds restricts which task kinds this worker claims. Empty means all.
	// This is how a multi-model pipeline is assembled: point one worker at
	// refine_ticket, another at implement_ticket, a third at review_pr.
	Kinds []string
	// GitHub may be nil to disable all GitHub side effects.
	GitHub *githubcli.Client
	// LocalOnly records that the operator *chose* to run without GitHub
	// (--no-github). Without this flag the worker cannot tell "deliberately
	// offline" from "gh is broken", and issue-triggered tasks would silently
	// succeed without ever updating the issue.
	LocalOnly bool
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
		// Long enough that a brief network blip does not lose the lease, and
		// renewed at a third of this interval while work is in progress.
		cfg.LeaseSeconds = 300
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
		Kinds:   w.cfg.Kinds,
	})
	if err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	handles := "all kinds"
	if len(worker.Kinds) > 0 {
		handles = strings.Join(worker.Kinds, ", ")
	}
	w.logger.Printf("registered as %s (name=%s runtime=%s handles=%s)",
		worker.ID, worker.Name, worker.Runtime, handles)
	w.logger.Printf("work dir: %s", w.cfg.WorkDir)

	go w.heartbeatLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			w.logger.Printf("shutting down")
			return nil
		default:
		}

		task, lease, err := w.api.Claim(w.id, w.cfg.LeaseSeconds, w.cfg.Kinds)
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

	// Renew the lease while we work. Without this the reaper cannot tell a
	// slow agent from a dead one, and a task that outlives its lease would be
	// handed to a second worker while this one is still editing files.
	lost := w.startRenewals(taskCtx, task.ID, lease, cancel)

	status, summary := api.StatusSucceeded, ""
	result, err := w.execute(taskCtx, task, logf)
	if err != nil {
		status = api.StatusFailed
		summary = err.Error()
		logf(api.EventError, "task failed: %v", err)
	} else {
		summary = result.Summary
	}

	// If the lease was lost, the task already belongs to someone else.
	// Completing it would either fail or stomp on their result.
	select {
	case <-lost:
		w.logger.Printf("task %s: lease lost, abandoning without completing (another worker owns it now)", task.ID)
		return
	default:
	}

	if _, err := w.api.Complete(task.ID, lease, status, summary); err != nil {
		w.logger.Printf("could not complete task %s: %v", task.ID, err)
		return
	}
	w.logger.Printf("task %s -> %s", task.ID, status)
}

// startRenewals keeps the lease alive until ctx ends. If the server says the
// lease is gone, it cancels ctx (stopping the runtime) and closes the returned
// channel.
func (w *Worker) startRenewals(ctx context.Context, taskID, lease string, cancel context.CancelFunc) <-chan struct{} {
	lost := make(chan struct{})

	// Renew comfortably before expiry so one slow or failed call is survivable.
	interval := time.Duration(w.cfg.LeaseSeconds) * time.Second / 3
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				err := w.api.RenewLease(taskID, lease, w.cfg.LeaseSeconds)
				if err == nil {
					continue
				}
				if errors.Is(err, client.ErrLeaseLost) {
					w.logger.Printf("task %s: lease lost (%v); stopping work", taskID, err)
					close(lost)
					cancel()
					return
				}
				// A transient network error is not proof the lease is gone;
				// keep trying until it either recovers or the lease expires.
				w.logger.Printf("task %s: lease renewal failed, will retry: %v", taskID, err)
			}
		}
	}()
	return lost
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
	if task.GitHubIssueNumber != nil {
		switch {
		case w.cfg.GitHub != nil:
			iss, skip, err := w.revalidateIssue(task, logf)
			if err != nil {
				return agentruntime.Result{}, err
			}
			if skip != "" {
				return agentruntime.Result{Summary: skip}, nil
			}
			issue = iss

		case w.cfg.LocalOnly:
			// The operator explicitly asked for no GitHub interaction, so
			// running without publishing is what they wanted. Say so loudly
			// in the task log rather than letting it look like a full success.
			logf(api.EventWarn,
				"local-only mode: issue #%d will NOT be commented on or relabelled",
				*task.GitHubIssueNumber)

		default:
			// Silently succeeding here would be the worst outcome: the task
			// looks done, but the issue is never updated and the label still
			// says it is waiting.
			return agentruntime.Result{}, fmt.Errorf(
				"task is for issue #%d but the gh CLI is unavailable, so the issue could not be updated; "+
					"authenticate with `gh auth login`, or pass --no-github to run local-only on purpose",
				*task.GitHubIssueNumber)
		}
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

	// A review works on the PR's code, not on the default branch, so it
	// branches from the PR head instead.
	promptText := task.Prompt
	var pr *githubcli.PullRequest
	if task.Kind == api.KindReviewPR {
		found, skip, err := w.resolvePRForReview(task, issue, logf)
		if err != nil {
			return agentruntime.Result{}, err
		}
		if skip != "" {
			return agentruntime.Result{Summary: skip}, nil
		}
		pr = found

		if ref := cache.BaseRef(pr.HeadRefName); ref != pr.HeadRefName {
			baseRef = ref
		} else {
			return agentruntime.Result{}, fmt.Errorf(
				"PR #%d's branch %q is not on the remote, so there is nothing to review; "+
					"the implementing worker must run with --push", pr.Number, pr.HeadRefName)
		}

		// Rebuild the prompt now that the PR and its diff actually exist.
		diff, err := w.cfg.GitHub.PRDiff(task.FullName(), pr.Number)
		if err != nil {
			return agentruntime.Result{}, fmt.Errorf("fetch PR diff: %w", err)
		}
		logf(api.EventInfo, "fetched diff for PR #%d (%d bytes)", pr.Number, len(diff))
		promptText = prompt.WithDiff(buildReviewPrompt(task, issue, pr), diff)
	}

	branch := BranchName(task)
	worktreeDir := w.worktreeDir(task)
	logf(api.EventInfo, "creating worktree %s on branch %s (from %s)", worktreeDir, branch, baseRef)

	wt, err := cache.AddWorktree(worktreeDir, branch, baseRef)
	if err != nil {
		return agentruntime.Result{}, fmt.Errorf("create worktree: %w", err)
	}

	// The prompt goes in the worktree so the agent can read it as a file, and
	// so it is visible in the diff-free workspace while debugging.
	promptFile := filepath.Join(worktreeDir, ".factory-task.md")
	if err := os.WriteFile(promptFile, []byte(promptText), 0o644); err != nil {
		return agentruntime.Result{}, fmt.Errorf("write prompt file: %w", err)
	}
	logf(api.EventInfo, "wrote prompt to .factory-task.md (%d bytes)", len(promptText))

	result, err := w.cfg.Runtime.Run(agentruntime.RunContext{
		Ctx:         ctx,
		Task:        task,
		WorktreeDir: worktreeDir,
		PromptFile:  promptFile,
		Prompt:      promptText,
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
		case api.KindReviewPR:
			if err := w.publishReview(task, *issue, *pr, result, logf); err != nil {
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

// BranchName returns the git branch for one *attempt* at a task.
//
// The attempt number matters. A retry reuses the task ID, so without it the
// second attempt would try to create a branch that already exists and a
// worktree in a directory that is already occupied — and fail before the agent
// ever runs. Attempt-scoping also keeps a failed attempt's work around for
// inspection instead of overwriting it.
func BranchName(task api.Task) string {
	return fmt.Sprintf("factory/task-%s-attempt-%d", task.ID, attemptNumber(task))
}

// worktreeDir returns the isolated checkout path for one attempt at a task.
func (w *Worker) worktreeDir(task api.Task) string {
	return filepath.Join(w.cfg.WorkDir, "worktrees", task.ID,
		fmt.Sprintf("attempt-%d", attemptNumber(task)))
}

// attemptNumber defends against a zero count, so paths never collide even if a
// task arrives without one.
func attemptNumber(task api.Task) int {
	if task.AttemptCount < 1 {
		return 1
	}
	return task.AttemptCount
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
