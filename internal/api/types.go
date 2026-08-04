// Package api holds the domain models and the JSON request/response shapes
// shared by the control plane server, the worker, and the CLI.
//
// Keeping these in one place means there is exactly one definition of "what a
// task looks like" for every process in the system.
package api

import "time"

// Task kinds.
const (
	KindManual          = "manual"
	KindRefineTicket    = "refine_ticket"
	KindImplementTicket = "implement_ticket"
	KindReviewPR        = "review_pr"
)

// Task statuses.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusLost      = "lost"
)

// Worker statuses.
const (
	WorkerActive = "active"
	WorkerStale  = "stale"
)

// Task event types. These are just strings in the database; the constants
// exist so the code does not scatter magic literals around.
const (
	EventCreated  = "created"
	EventClaimed  = "claimed"
	EventLog      = "log"
	EventInfo     = "info"
	EventWarn     = "warn"
	EventError    = "error"
	EventGitHub   = "github"
	EventComplete = "complete"
)

// ValidKinds reports whether kind is a task kind the system understands.
func ValidKinds() []string {
	return []string{KindManual, KindRefineTicket, KindImplementTicket, KindReviewPR}
}

// IsValidKind reports whether kind is one of ValidKinds.
func IsValidKind(kind string) bool {
	for _, k := range ValidKinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// IsTerminal reports whether a task status means the task will never run again.
func IsTerminal(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusLost:
		return true
	}
	return false
}

// Worker is one worker process identity. One worker owns one runtime.
type Worker struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Runtime    string    `json:"runtime"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

// Repository is a git repository the factory is allowed to work on.
type Repository struct {
	ID             int64     `json:"id"`
	Owner          string    `json:"owner"`
	Name           string    `json:"name"`
	CloneURL       string    `json:"clone_url"`
	LocalCachePath string    `json:"local_cache_path"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
}

// FullName returns "owner/name".
func (r Repository) FullName() string { return r.Owner + "/" + r.Name }

// Task is one unit of agent work.
type Task struct {
	ID                string     `json:"id"`
	Kind              string     `json:"kind"`
	Status            string     `json:"status"`
	RepoOwner         string     `json:"repo_owner"`
	RepoName          string     `json:"repo_name"`
	GitHubIssueNumber *int       `json:"github_issue_number,omitempty"`
	Title             string     `json:"title"`
	Prompt            string     `json:"prompt"`
	WorkerID          *string    `json:"worker_id,omitempty"`
	LeaseToken        *string    `json:"lease_token,omitempty"`
	LeaseExpiresAt    *time.Time `json:"lease_expires_at,omitempty"`
	AttemptCount      int        `json:"attempt_count"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// FullName returns "owner/name" for the task's repository.
func (t Task) FullName() string { return t.RepoOwner + "/" + t.RepoName }

// TaskEvent is one log line or state transition attached to a task.
type TaskEvent struct {
	ID        int64     `json:"id"`
	TaskID    string    `json:"task_id"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// GitHubObservation records that we already turned a given GitHub issue into a
// task for a given workflow. The dedupe key is unique, which is what stops
// repeated polling from creating duplicate tasks.
type GitHubObservation struct {
	ID           int64     `json:"id"`
	RepoOwner    string    `json:"repo_owner"`
	RepoName     string    `json:"repo_name"`
	IssueNumber  int       `json:"issue_number"`
	WorkflowKind string    `json:"workflow_kind"`
	DedupeKey    string    `json:"dedupe_key"`
	TaskID       *string   `json:"task_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// ---------- Request / response payloads ----------

// CreateTaskRequest is the body of POST /tasks.
type CreateTaskRequest struct {
	Kind              string `json:"kind"`
	RepoOwner         string `json:"repo_owner"`
	RepoName          string `json:"repo_name"`
	Title             string `json:"title"`
	Prompt            string `json:"prompt"`
	GitHubIssueNumber *int   `json:"github_issue_number,omitempty"`
}

// CreateRepositoryRequest is the body of POST /repositories.
type CreateRepositoryRequest struct {
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	CloneURL string `json:"clone_url"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

// RegisterWorkerRequest is the body of POST /workers/register.
type RegisterWorkerRequest struct {
	// ID is optional. Workers pass a stable ID so that restarting a worker
	// keeps its identity instead of leaking a new row every boot.
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
}

// HeartbeatRequest is the body of POST /workers/heartbeat.
type HeartbeatRequest struct {
	WorkerID string `json:"worker_id"`
}

// ClaimRequest is the body of POST /tasks/claim.
type ClaimRequest struct {
	WorkerID string `json:"worker_id"`
	// LeaseSeconds is optional; the server applies a default when it is zero.
	LeaseSeconds int `json:"lease_seconds,omitempty"`
}

// ClaimResponse is returned by POST /tasks/claim when a task was claimed.
type ClaimResponse struct {
	Task       Task   `json:"task"`
	LeaseToken string `json:"lease_token"`
}

// AppendEventRequest is the body of POST /tasks/{id}/events.
type AppendEventRequest struct {
	LeaseToken string `json:"lease_token"`
	Type       string `json:"type"`
	Message    string `json:"message"`
}

// CompleteTaskRequest is the body of POST /tasks/{id}/complete.
type CompleteTaskRequest struct {
	LeaseToken string `json:"lease_token"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}

// PollResponse is returned by POST /github/poll.
type PollResponse struct {
	Repositories   int      `json:"repositories"`
	IssuesSeen     int      `json:"issues_seen"`
	TasksCreated   int      `json:"tasks_created"`
	Skipped        int      `json:"skipped_duplicates"`
	CreatedTaskIDs []string `json:"created_task_ids"`
	Errors         []string `json:"errors,omitempty"`
	DryRun         bool     `json:"dry_run"`
}

// ErrorResponse is the uniform error body for every endpoint.
type ErrorResponse struct {
	Error string `json:"error"`
}
