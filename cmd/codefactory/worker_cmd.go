package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HoaViet-Tech/factory/internal/githubcli"
	agentruntime "github.com/HoaViet-Tech/factory/internal/runtime"
	"github.com/HoaViet-Tech/factory/internal/worker"
)

func runWorker(args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	serverURL := fs.String("server", defaultServer, "control plane URL")
	name := fs.String("name", "local", "human-readable worker name")
	id := fs.String("id", "", "stable worker ID (default: persisted in <work-dir>/worker-id)")
	runtimeName := fs.String("runtime", agentruntime.Fake, "runtime: fake, shell, codex or claude")
	runtimeCmd := fs.String("runtime-command", "", "command template for shell/codex/claude runtimes ({{prompt_file}}, {{worktree}}, {{task_id}})")
	runtimeStdin := fs.Bool("runtime-stdin", false, "feed the prompt to the runtime command on stdin")
	workDir := fs.String("work-dir", ".factory", "directory for the repo cache, worktrees and worker id")
	push := fs.Bool("push", false, "push the task branch and open a draft PR when the agent produces changes")
	dryRun := fs.Bool("github-dry-run", false, "log GitHub writes instead of performing them")
	noGitHub := fs.Bool("no-github", false, "disable all GitHub interaction, even for issue-triggered tasks")
	once := fs.Bool("once", false, "exit after finishing one task (or immediately if the queue is empty)")
	taskTimeout := fs.Duration("task-timeout", 30*time.Minute, "maximum runtime duration for a single task")
	leaseSeconds := fs.Int("lease-seconds", 120, "requested lease duration in seconds")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Run a worker that claims tasks and executes them in isolated git worktrees.\n\nUsage: codefactory worker [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := log.New(os.Stdout, "[worker] ", log.LstdFlags)

	rt, err := buildRuntime(*runtimeName, *runtimeCmd, *runtimeStdin)
	if err != nil {
		return err
	}

	// GitHub is only needed for issue-triggered tasks. A fake-runtime demo
	// runs happily without it.
	var gh *githubcli.Client
	if !*noGitHub {
		client := githubcli.New(*dryRun)
		client.Logf = func(format string, args ...any) { logger.Printf(format, args...) }
		if err := client.Available(); err != nil {
			logger.Printf("GitHub actions disabled: %v", err)
		} else {
			gh = client
			logger.Printf("GitHub actions enabled (dry-run=%v, push=%v)", *dryRun, *push)
		}
	}
	if *push && gh == nil {
		logger.Printf("warning: --push has no effect because GitHub is unavailable")
	}

	w, err := worker.New(worker.Config{
		ServerURL:    *serverURL,
		Name:         *name,
		ID:           *id,
		WorkDir:      *workDir,
		Runtime:      rt,
		GitHub:       gh,
		Push:         *push,
		Once:         *once,
		TaskTimeout:  *taskTimeout,
		LeaseSeconds: *leaseSeconds,
		Logger:       logger,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return w.Run(ctx)
}

// buildRuntime turns the --runtime flags into a Runtime, failing early and
// loudly when a real agent CLI is missing.
func buildRuntime(name, command string, stdin bool) (agentruntime.Runtime, error) {
	switch name {
	case agentruntime.Fake:
		return agentruntime.FakeRuntime{}, nil

	case agentruntime.Shell:
		if command == "" {
			return nil, fmt.Errorf("--runtime shell requires --runtime-command")
		}
		rt := &agentruntime.CommandRuntime{RuntimeName: name, Template: command, Stdin: stdin}
		if err := rt.Available(); err != nil {
			return nil, err
		}
		return rt, nil

	case agentruntime.Codex, agentruntime.Claude:
		rt, err := agentruntime.NewCommandRuntime(name, command, stdin)
		if err != nil {
			return nil, err
		}
		if err := rt.Available(); err != nil {
			return nil, err
		}
		return rt, nil

	default:
		return nil, fmt.Errorf("unknown runtime %q (valid: fake, shell, codex, claude)", name)
	}
}
