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

**Renewal is what makes this safe for slow agents.** A worker calls
`POST /tasks/{id}/renew` every third of its lease while it works. Without that,
the reaper cannot distinguish "still working" from "dead", so any task
outliving its lease would be requeued and run *concurrently* by a second
worker while the first is still editing files. If a renewal comes back 409, the
worker has lost the task: it cancels the running agent and does not complete
the task, because it no longer owns it.

Note the ordering guarantee: `RenewLease` goes through the same `checkLease`
as completion, so a worker whose lease was already reaped cannot resurrect it.

**Requeued tasks back off before they can be claimed again.** The reaper sets
`run_after = now + RetryBackoff(attempt)` (30s, then 60s, capped at 10m), and
the claim query skips anything still inside that window. Without it, a
transient failure — a GitHub rate limit, a flaky network — burns all three
attempts within seconds and lands in `lost`, when waiting would have worked.
The backoff is deterministic rather than jittered: there is one control plane,
so there is no thundering herd to scatter, and determinism makes it testable.

Note that this applies to *expired leases* only. A task whose worker reports
`failed` is terminal and is never retried: the worker got far enough to have an
opinion, and repeating a deterministic failure helps nobody.

### Attempts are isolated, not just tasks

A retry reuses the task ID, so branch and worktree names are scoped by
**attempt**: `factory/task-<id>-attempt-<n>` in
`<work-dir>/worktrees/<task-id>/attempt-<n>`.

Without that scoping, attempt 2 would try to create a branch that already
exists and a worktree in an occupied directory, and would fail before the agent
ever ran — turning one flaky attempt into a permanently stuck task. Keeping
each attempt separate also preserves the failed attempt's work for inspection.

### Dedupe: the second core idea

Polling is at-least-once. Task creation must be exactly-once.

`CreateTaskForIssue` inserts the observation and the task in a single
transaction, keyed by `owner/name#number:workflow_kind` with a UNIQUE
constraint. Poll an issue a hundred times and you get one task. Poll it for a
*different* workflow (refine vs implement) and you correctly get a second one.

### Routing: how one queue becomes a pipeline

A worker registers with the task kinds it accepts (`workers.kinds`), and
`ClaimTask` filters on them. Empty means "anything", so a single-worker setup
is unchanged.

This is what makes multi-model pipelines work. Without it the queue is FIFO
across all kinds, so a worker running an expensive coding model would happily
pick up a cheap refine task — and worse, a refine-only worker would pick up an
implement task it has the wrong prompt and model for.

The claim request may narrow further, but if it says nothing the server falls
back to the kinds recorded at registration, so routing survives a worker that
forgets to ask.

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

### Two guards that are not authentication

The API is unauthenticated by design (see "Remote access" in the README). Two
checks still sit in front of it, because they close paths that do not need a
stolen credential at all.

**The browser guard** (`browserguard.go`). Cross-site request forgery survives
every network control in this project: a tunnel or a firewall keeps other
machines out, but the browser on the same machine is already inside. Any page
you visit can make your browser POST to `127.0.0.1:7337`; it cannot read the
reply, but the task is created regardless.

So state-changing requests must carry `Content-Type: application/json` and must
not carry a foreign `Origin`. Browsers cannot send a JSON body cross-origin
without a CORS preflight, which nothing here answers, and they always stamp
`Origin` on cross-site requests while CLIs never do. GET is deliberately left
open: it changes nothing, and keeping the API browsable by hand is useful.

**Clone URL validation** (`api/cloneurl.go`). `git clone` accepts inputs that
are not addresses:

- `ext::sh -c '...'` is git's remote-helper syntax and *runs a command*.
- `--upload-pack=...` starts with a dash, so git reads it as a flag.

Either turns "register a repository" into "execute this". Validation happens in
`AddRepository`, so a bad URL never reaches the database, let alone git.

Neither guard is a substitute for the network boundary. They close the two
routes that work without any credential; the boundary is what stops everything
else.

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
`--github-dry-run` mode. **Dry-run is not an offline mode** — reading issues is
a real API call, so it still needs an authenticated `gh`. For genuinely
credential-free work, use the fake runtime against a local repository
(`docs/DEMO.md`).

If a task carries a `github_issue_number` but no GitHub client is available,
the task **fails** rather than succeeding without updating the issue. The
alternative — a green task next to an issue still labelled `factory:inbox` —
is the kind of silent divergence that destroys trust in an automation. Passing
`--no-github` opts into local-only execution deliberately, and the task log
warns that the issue was not touched.

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

### The review stage

`review_pr` is the one stage that does not work on the default branch.

1. The poller sees `factory:review` and creates the task. Its prompt is a
   **placeholder**: the PR and its diff do not exist at poll time.
2. The worker finds the PR by searching open PRs for one whose body references
   the issue (`Closes #N`, written by the implement stage). Matching on the body
   rather than the branch name means a hand-written PR is reviewable too.
3. It branches the worktree from the **PR head** instead of the default branch,
   so the reviewing agent can read the surrounding code, not just the diff.
4. It fetches the live diff with `gh pr diff` and rebuilds the prompt.
5. The runtime writes `.factory-review.md`.
6. The worker posts it as a PR comment and, on `REQUEST_CHANGES`, moves the
   issue to `factory:blocked`.

Three deliberate choices here:

**Reviews are ordinary comments, never GitHub review approvals.** A formal
approval can satisfy branch protection. An agent must not be able to do that.

**`ParseVerdict` fails closed.** An empty, truncated or unparseable review
resolves to `COMMENT`, never `APPROVE`, and it only reads inside the `##
Verdict` section so the word "approve" in prose cannot flip the outcome.

**The fake reviewer never returns `APPROVE`.** It performs mechanical checks
only — added-line greps and "did any test file change?" — and says so in the
comment it posts. A rubber stamp from something that read nothing is worse than
no review.

Known limitation: dedupe is per issue and workflow, so an issue gets **one**
review, not a fresh one after each push. Re-reviewing on new commits needs a
dedupe key that includes the head SHA.

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
   ├─ REFINER claims A ──▶ re-reads issue ──▶ still factory:inbox? ──▶ +factory:refining
   │      └─ worktree ──▶ runtime ──▶ .factory-refined.md
   │      └─ comment ticket, -factory:inbox -factory:refining, +factory:ready
   │
   ├─ server polls  ──▶ dedupe key local/demo#7:implement_ticket ──▶ task B queued
   │
   ├─ CODER claims B ──▶ still factory:ready? ──▶ +factory:active
   │      └─ worktree ──▶ runtime edits files ──▶ commit
   │      └─ (--push) push branch ──▶ draft PR ──▶ comment link ──▶ +factory:review
   │
   ├─ server polls  ──▶ dedupe key local/demo#7:review_pr ──▶ task C queued
   │
   └─ REVIEWER claims C ──▶ finds the PR ──▶ worktree at PR head ──▶ gh pr diff
          └─ runtime ──▶ .factory-review.md ──▶ comment on the PR
          └─ REQUEST_CHANGES? ──▶ -factory:review +factory:blocked
```

Each stage is claimed by a worker that declared that kind, so REFINER, CODER
and REVIEWER can be three different models. At every arrow a crash means the
lease expires and the task is retried on a fresh attempt branch. At every
GitHub mutation, the label is checked live first.

## Deliberate omissions

| Not built | Why |
|---|---|
| Auth on the API | Deliberate: it binds to loopback and is reached over a tunnel. See "Remote access" in the README — an exposed port here is remote code execution |
| Multi-node | `SetMaxOpenConns(1)` and a local file assume one machine |
| Automatic merging | A human reviews. Always. |
| Re-review after new commits | Dedupe is per issue+workflow; needs a head-SHA key |
| Worktree cleanup | Every task and retry leaves a checkout on disk, on purpose |
