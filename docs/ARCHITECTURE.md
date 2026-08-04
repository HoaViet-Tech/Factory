# Architecture

This document explains what each piece does and, more importantly, *why* it
exists. Read it alongside the code; every section names the files involved.

## The shape of the system

Two processes share one SQLite database through an HTTP API:

```
  CLI ──────┐
            ├──HTTP──▶ control plane ──▶ SQLite (factory.db)
  worker ───┘              │
                           └── gh CLI ──▶ GitHub
  worker ──▶ git ──▶ repo cache ──▶ worktrees
```

The worker never touches the database directly. That constraint is doing real
work: it forces every state transition through an API that can be tested,
logged, and eventually run on another machine.

## 1. The store — `internal/store`

SQLite, five tables, plain SQL.

| Table | Holds |
|---|---|
| `workers` | Worker identity, runtime, heartbeat |
| `repositories` | What the factory may work on |
| `tasks` | The queue and every task's state |
| `task_events` | Append-only log per task |
| `github_observations` | "We already reacted to this issue" |

Three decisions worth understanding:

**Times are RFC3339 strings.** Slower than integers, but you can open the
database with any SQLite browser and read it. For a learning project that
matters more than microseconds.

**One connection** (`SetMaxOpenConns(1)`). Writes serialise, and a whole class
of "database is locked" bugs disappears. This is the single-node assumption
made explicit.

**Migrations are an append-only list** (`schema.go`). Adding a change means
adding an entry, never editing an old one.

### Leases: the core idea

A queued task is claimed atomically inside one transaction (`ClaimTask`):
status → `running`, a random `lease_token` is stamped on the row,
`lease_expires_at` is set, and `attempt_count` increments.

The worker must present that token to append events or to complete the task.
A worker whose lease was reaped therefore *cannot* corrupt a task another
worker has since picked up.

`ReapExpiredLeases` runs on a ticker in the server. Expired leases go back to
`queued`, and after `MaxAttempts` (3) become `lost`. That is the entire crash
recovery story, and it needs no supervisor, no heartbeat timeout logic in the
queue, and no distributed lock.

### Dedupe: the second core idea

Polling is at-least-once. Task creation must be exactly-once.

`CreateTaskForIssue` inserts the observation and the task in a single
transaction, keyed by `owner/name#number:workflow_kind` with a UNIQUE
constraint. Poll an issue a hundred times and you get one task. Poll it for a
*different* workflow (refine vs implement) and you correctly get a second one.

## 2. The control plane — `internal/server`

A `net/http.ServeMux` using Go 1.22+ method+pattern routing, a thin JSON
layer, and two background loops (lease reaping, optional GitHub polling).

`writeStoreErr` maps the store's sentinel errors onto status codes in one
place, so every handler reports failures identically:

| Store error | HTTP |
|---|---|
| `ErrNotFound` | 404 |
| `ErrInvalidLease` | 409 |
| `ErrNotRunning` | 409 |
| `ErrTerminal` | 409 |

An empty queue returns **204**, not an error — idle is the normal case for a
polling worker, and it should not look like a failure in the logs.

## 3. The worker — `internal/worker`

The loop is five lines of intent: register → heartbeat → claim → execute →
complete. Everything else hangs off that spine.

Per claimed task, `execute` does:

1. **Revalidate** the GitHub issue if the task came from one (see below).
2. **Cache the repo** — clone once into `<work-dir>/repos/owner/name`, fetch
   afterwards. A failed fetch is a warning, not an error, so you can work
   offline.
3. **Branch + worktree** — `factory/task-<id>` checked out into
   `<work-dir>/worktrees/<task-id>` via `git worktree add`.
4. **Write the prompt** to `.factory-task.md` inside the worktree.
5. **Run the runtime** with stdout and stderr streamed into `task_events` as
   they arrive.
6. **Inspect `git status`**.
7. **Publish** GitHub side effects.
8. **Complete** the task.

### Why worktrees rather than clones

A worktree is a second checkout sharing one object database. Creating one is
near-instant and costs almost no disk, so N concurrent agents can each have a
private filesystem without N full clones. The isolation is what makes running
several agents on one repository safe.

Worktrees are deliberately **left on disk** after a task finishes, so you can
inspect exactly what the agent did. Clean them up with `git worktree prune`
after deleting the directories.

### Worker identity

A worker persists its ID in `<work-dir>/worker-id`. Restarting keeps the same
identity instead of leaking a new row per boot. One worker ID owns exactly one
runtime — that is why `--runtime` is a worker-level flag, not a per-task field.

## 4. Runtimes — `internal/runtime`

```go
type Runtime interface {
    Name() string
    Run(rc RunContext) (Result, error)
}
```

A runtime gets a prepared worktree and a prompt file, and may change anything
inside that directory.

- **`fake`** is not a mock. It writes real files into the real worktree, which
  produces a real diff. That is what makes the whole pipeline demoable and
  testable without credentials, and it is the first thing you should get
  working when rebuilding this.
- **`shell` / `codex` / `claude`** run a command template through the platform
  shell, with `{{prompt_file}}`, `{{worktree}}` and `{{task_id}}` substituted.
  Missing binaries are detected at **startup**, not mid-task.

For refine tasks the contract is a file: the runtime writes
`.factory-refined.md`. If it does not, the outcome is `needs-human` rather
than a crash — a vague answer from an agent is a product state, not an error.

## 5. GitHub — `internal/githubcli`, `internal/ingest`, `internal/labels`

**Why `gh` instead of a GitHub App:** it reuses credentials you already have,
and every call is a command you could have typed yourself. Debugging is
`gh issue view 5 --json labels`, not a webhook replay.

Read calls always execute. Write calls are skipped and logged in
`--github-dry-run` mode.

Responsibility split:

- The **server** polls and creates tasks (`internal/ingest`). It never mutates
  GitHub.
- The **worker** performs mutations, after doing the work.

### Live revalidation

The poll snapshot may be minutes old. Before touching an issue the worker
re-fetches it and checks the trigger label is still present
(`internal/worker/github.go`). If a human removed the label, closed the issue,
or already handled it, the task completes successfully having done nothing,
and says why in its log.

Without this, "I changed my mind and removed the label" would still result in
an agent opening a PR.

## 6. Prompt safety — `internal/prompt`

Issue bodies are written by anyone on the internet. They are **data**.

Every issue body enters a prompt through `WrapUntrusted`, which fences it in
long, unusual markers, states plainly that the content must not be obeyed, and
strips forged copies of the markers so the fence cannot be closed early.

This is not complete protection against prompt injection — nothing is — but it
is the baseline, and it is tested (`prompt_test.go`).

## Data flow: one issue, end to end

```
human labels issue factory:inbox
   │
   ├─ server polls  ──▶ dedupe key local/demo#7:refine_ticket  ──▶ task A queued
   │
   ├─ worker claims A ──▶ re-reads issue ──▶ still factory:inbox? ──▶ +factory:refining
   │      └─ worktree ──▶ runtime ──▶ .factory-refined.md
   │      └─ comment ticket, -factory:inbox -factory:refining, +factory:ready
   │
   ├─ server polls  ──▶ dedupe key local/demo#7:implement_ticket ──▶ task B queued
   │
   └─ worker claims B ──▶ still factory:ready? ──▶ +factory:active
          └─ worktree ──▶ runtime edits files ──▶ commit
          └─ (--push) push branch ──▶ draft PR ──▶ comment link ──▶ +factory:review
```

At every arrow, a crash means the lease expires and the task is retried. At
every GitHub mutation, the label is checked live first.

## Deliberate omissions

| Not built | Why |
|---|---|
| Lease renewal mid-task | Long tasks just use a longer lease; renewal is the natural next step |
| Auth on the API | It binds to 127.0.0.1; add auth before it ever leaves localhost |
| Multi-node | `SetMaxOpenConns(1)` and a local file assume one machine |
| Automatic merging | A human reviews. Always. |
| `review_pr` workflow | The kind exists in the schema; no poller or handler yet |
