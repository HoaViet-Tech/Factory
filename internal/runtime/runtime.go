// Package runtime is the pluggable "thing that actually does the work" layer.
//
// A runtime receives a prepared worktree and a prompt file and is free to
// change files inside that worktree. The fake runtime does so deterministically
// with no LLM at all, which is what makes the whole system demoable and
// testable without credentials.
package runtime

import (
	"context"

	"github.com/HoaViet-Tech/factory/internal/api"
)

// Names of the built-in runtimes.
const (
	Fake   = "fake"
	Shell  = "shell"
	Codex  = "codex"
	Claude = "claude"
)

// RunContext is everything a runtime is given.
type RunContext struct {
	Ctx context.Context
	// Task is the claimed task.
	Task api.Task
	// WorktreeDir is an isolated checkout on a fresh branch. A runtime may
	// modify anything inside it and nothing outside it.
	WorktreeDir string
	// PromptFile is the absolute path to .factory-task.md inside the worktree.
	PromptFile string
	// Prompt is the same content, already in memory.
	Prompt string
	// Log streams a line back to the control plane.
	Log func(format string, args ...any)
}

// Result is what a runtime reports back.
type Result struct {
	// Summary is a short human-readable outcome, used in issue comments.
	Summary string
	// RefinedTicket is the structured ticket produced by a refine task.
	RefinedTicket string
	// Review is the review document produced by a review task.
	Review string
	// Verdict is the review verdict (APPROVE, REQUEST_CHANGES, COMMENT).
	Verdict string
	// NeedsHuman marks a refine result as too ambiguous to implement.
	NeedsHuman bool
	// Reason explains NeedsHuman, or why an implement task produced nothing.
	Reason string
}

// Runtime executes one task inside a worktree.
type Runtime interface {
	// Name identifies the runtime, and is stored on the worker row.
	Name() string
	// Run performs the work. Returning an error fails the task.
	Run(rc RunContext) (Result, error)
}
