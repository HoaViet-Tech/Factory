# Code Factory

A local-first control plane for AI coding agents. GitHub issues become agent
tasks: vague tickets get refined into structured ones, and refined tickets get
implemented by an agent working in an isolated git worktree, which can open a
draft PR.

It runs on your machine, stores everything in one SQLite file, and talks to
GitHub through the `gh` CLI you already have.

This is a **learning project**. It is deliberately small, explicit, and
readable. It takes architectural inspiration from
[owainlewis/factory](https://github.com/owainlewis/factory) — the control
plane / worker split, SQLite state, leases, worktrees, GitHub automations —
but the code here is written from scratch to be studied and rebuilt.

## The idea in one picture

```
    GitHub issue                 control plane                    worker
  ┌───────────────┐          ┌──────────────────┐          ┌────────────────┐
  │ factory:inbox │──poll───▶│  task queue      │◀──claim──│ stable ID      │
  └───────────────┘          │  (SQLite)        │          │ one runtime    │
                             │  leases          │          │ git worktree   │
  ┌───────────────┐          │  events / logs   │──lease──▶│ runs the agent │
  │ factory:ready │──poll───▶│  worker registry │          │ streams logs   │
  └───────────────┘          └──────────────────┘          └────────────────┘
          ▲                                                        │
          └──────────── comments, labels, draft PR ◀────────────────┘
```

Two long-running processes, one binary:

- **`codefactory server`** — HTTP API, SQLite, task queue, worker registry,
  lease reaper, GitHub issue polling.
- **`codefactory worker`** — claims one task at a time, prepares an isolated
  worktree, runs a runtime inside it, streams logs back, completes the task.

## Quick start (no GitHub credentials needed)

```bash
go build -o codefactory ./cmd/codefactory
```

Start the control plane:

```bash
./codefactory server --db ./factory.db --listen 127.0.0.1:7337
```

In a second terminal, point it at any local git repository and queue some work:

```bash
./codefactory repo add local/demo --clone-url /path/to/a/local/repo
```

```bash
./codefactory task create --repo local/demo --title "Try the factory" --prompt "Leave a note in the worktree"
```

In a third terminal, run a worker:

```bash
./codefactory worker --server http://127.0.0.1:7337 --name local-fake --runtime fake --once
```

Then look at what happened:

```bash
./codefactory task list
```

Full walkthrough with expected output: **[docs/DEMO.md](docs/DEMO.md)**.

## The GitHub workflow

Label an issue and the factory picks it up:

| Label | Meaning |
|---|---|
| `factory:inbox` | A human wants this refined |
| `factory:refining` | A refine task is in flight |
| `factory:needs-human` | Too ambiguous to implement safely |
| `factory:ready` | Refined; safe to implement |
| `factory:active` | An implement task is in flight |
| `factory:review` | A draft PR is open; the review stage picks this up |
| `factory:blocked` | The agent could not finish |
| `factory:done` | Finished |

The three flows:

1. **Refine.** Poll `factory:inbox` → create one `refine_ticket` task →
   worker re-reads the live issue, runs the runtime, comments a structured
   ticket back, removes `factory:inbox`, and adds either `factory:ready` or
   `factory:needs-human`.
2. **Implement.** Poll `factory:ready` → create one `implement_ticket` task →
   worker branches, works in a worktree, and if files changed, commits and
   (with `--push`) opens a **draft** PR, then labels the issue
   `factory:review`.
3. **Review.** Poll `factory:review` → create one `review_pr` task → worker
   finds the PR, checks out its head in a worktree, fetches the diff, and posts
   a review comment. `REQUEST_CHANGES` moves the issue to `factory:blocked`.

Bootstrap the labels once, then poll:

```bash
./codefactory github labels --repo owner/name
```

```bash
./codefactory github poll --server http://127.0.0.1:7337
```

## Multi-agent pipelines

A worker claims only the task kinds it declares, so you assign a different
model to each stage by running one worker per stage:

```bash
./codefactory worker --name refiner  --kinds refine_ticket    --runtime claude --runtime-command "claude --print --model claude-haiku-4-5-20251001" --runtime-stdin
```

```bash
./codefactory worker --name coder    --kinds implement_ticket --runtime claude --push
```

```bash
./codefactory worker --name reviewer --kinds review_pr        --runtime codex
```

Omit `--kinds` and a worker takes anything, which is what a single-worker setup
wants.

Why bother: **the review stage is worth a different model than the implement
stage.** A model reviewing its own output shares its own blind spots — if it
misread the requirement while coding, it will misread it the same way while
reviewing. Different providers give genuinely independent failure modes.
Refine-vs-implement matters much less; use a cheap model there because it runs
on every inbox issue.

Check what each worker is handling:

```bash
curl -s localhost:7337/workers | jq '.[] | {name, runtime, kinds}'
```

Two constraints worth knowing before you wire this up:

- **Each stage is a CLI invocation that edits files in a directory.** A model
  needs a headless CLI to participate; a chat UI cannot.
- **Reviews are posted as ordinary PR comments, never as GitHub approvals.** An
  agent must not be able to satisfy a human approval requirement on a protected
  branch.

## Runtimes

One worker owns exactly one runtime.

| `--runtime` | What it does |
|---|---|
| `fake` | Deterministic, no LLM, no credentials. Writes a real file so the worktree really changes. Use this to learn the system. |
| `shell` | Runs any command you give it via `--runtime-command`. |
| `codex` | Runs `codex exec -- {{prompt_file}}`. |
| `claude` | Runs `claude --print` with the prompt on stdin. |

The agent CLIs move fast, so the commands are templates rather than hard-coded
flags. Override them:

```bash
./codefactory worker --runtime claude --runtime-command "claude --print --permission-mode acceptEdits" --runtime-stdin
```

If the binary is missing, the worker refuses to start and tells you to use
`--runtime fake` instead.

## Safety rules the code actually enforces

- **Never auto-merge.** PRs are always created with `--draft`.
- **Never delete branches**, and no destructive git commands anywhere.
- **Issue text is data, not instructions.** Every issue body is fenced in an
  untrusted block that tells the agent not to obey it, and forged fence
  markers are stripped ([internal/prompt](internal/prompt/prompt.go)).
- **Labels are revalidated live** immediately before any GitHub mutation, so a
  stale poll snapshot never causes an unwanted action.
- **GitHub-triggered tasks are deduplicated** by a unique key, so polling the
  same issue a hundred times creates exactly one task.
- **Leases** mean a dead worker cannot hold a task forever: an expired lease is
  requeued, and after 3 attempts the task is marked `lost`. A working worker
  **renews** its lease on a timer, so a slow agent is never mistaken for a dead
  one and handed to a second worker.
- **Retries back off** (30s, then 60s), so a rate limit or a network blip does
  not burn every attempt in the same five seconds.
- **Each attempt is isolated.** A retry gets its own branch and worktree
  (`factory/task-<id>-attempt-<n>`), so it never collides with the leftovers of
  the attempt that failed.
- **Issue-triggered tasks fail loudly if GitHub is unreachable**, rather than
  reporting success while the issue is never updated. Pass `--no-github` when
  you mean it.
- **Pushing is opt-in** (`--push`), and dry-run mode (`--github-dry-run`) logs
  every GitHub write instead of performing it.
- **Web pages cannot drive the control plane.** State-changing requests must be
  `application/json` and must not carry a foreign `Origin`, so a page you visit
  cannot POST tasks to your own localhost.
- **Clone URLs are validated.** `git clone` accepts inputs that are really
  commands — `ext::sh -c ...` runs a shell, and a leading `-` becomes a git
  flag. Both are rejected before they reach git.

## Remote access

**The API has no authentication, and it must not be exposed.**

That is not an oversight to work around — it follows from what the API does.
`POST /tasks` accepts an arbitrary prompt, and a worker feeds that prompt to an
agent that edits files and can push with your GitHub credentials. Anyone who
can reach the port can run code on your machine as you.

So the control plane binds to `127.0.0.1`, and you reach it from elsewhere
through a tunnel rather than by opening the port. The server prints a loud
warning if you ever bind it to a non-loopback address.

**SSH port forwarding** — nothing to install:

```bash
ssh -N -L 7337:127.0.0.1:7337 you@factory-host
```

The control plane is now at `http://127.0.0.1:7337` on your local machine, and
every CLI command works unchanged.

**Tailscale or another private network** — better if you want it always on.
Bind the server to the private interface only, never to `0.0.0.0`:

```bash
./codefactory server --listen 100.x.y.z:7337
```

That address is reachable only inside your tailnet. The startup warning will
still fire, because the code cannot tell a tailnet address from a public one —
in this case it is telling you to be sure, not that you are wrong.

Rules of thumb:

- Never `--listen 0.0.0.0:7337`, and never port-forward 7337 on a router.
- Run workers on the same host as the repositories they build.
- If you ever do need real exposure, put a reverse proxy with TLS **and**
  authentication in front of it. A bearer token over plain HTTP is not enough.

### The threat a tunnel does not stop

A tunnel keeps other *machines* away from the control plane. It does nothing
about the browser on the same machine: any page you visit can make your browser
POST to `127.0.0.1:7337`, and the page never needs to read the reply for the
task to be created.

That is why state-changing requests must send `Content-Type: application/json`
and must not carry a cross-origin `Origin` header. Browsers cannot send JSON
cross-origin without asking permission first, and this server never grants it.
The CLI and workers are unaffected — they already send exactly that.

If you write your own client, send `Content-Type: application/json` on every
POST and no `Origin` header:

```bash
curl -X POST -H 'Content-Type: application/json' -d '{"repo_owner":"you","repo_name":"repo","title":"hi","prompt":"hi"}' http://127.0.0.1:7337/tasks
```

## API

| Method | Path | Purpose |
|---|---|---|
| GET | `/healthz` | Liveness |
| POST | `/tasks` | Create a task |
| GET | `/tasks` | List tasks (`?status=&kind=&repo=&limit=`) |
| GET | `/tasks/{id}` | Show one task |
| POST | `/tasks/{id}/cancel` | Cancel a task |
| GET | `/tasks/{id}/events` | Task log |
| POST | `/workers/register` | Register a worker |
| POST | `/workers/heartbeat` | Worker liveness |
| GET | `/workers` | List workers |
| POST | `/tasks/claim` | Claim a task (204 when the queue is empty) |
| POST | `/tasks/{id}/events` | Append a log line (lease required) |
| POST | `/tasks/{id}/renew` | Extend the lease while working (lease required) |
| POST | `/tasks/{id}/complete` | Finish a task (lease required) |
| POST | `/repositories` | Register a repository |
| GET | `/repositories` | List repositories |
| POST | `/github/poll` | Run one poll now |

## Tests

```bash
go test ./...
```

They cover the task API, worker claim/complete, lease enforcement and expiry,
GitHub dedupe, repository parsing, prompt safety, and a full end-to-end slice
that creates a real git repository, runs a real worker, and checks the isolated
worktree. No network or credentials required (git-dependent tests skip if `git`
is missing).

## Documentation

- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — how the pieces fit and why
- **[docs/DEMO.md](docs/DEMO.md)** — the credential-free demo, plus the GitHub one
- **[docs/LEARNING.md](docs/LEARNING.md)** — rebuild this yourself, milestone by milestone

## Layout

```
cmd/codefactory/      CLI: server, worker, repo, task, github
internal/api/         Domain models + wire types (one definition each)
internal/store/       SQLite: schema, tasks, workers, repos, events, dedupe
internal/server/      HTTP control plane + background loops
internal/client/      HTTP client used by the worker and the CLI
internal/worker/      Claim → worktree → runtime → complete
internal/runtime/     fake / shell / codex / claude
internal/gitx/        Clone cache, worktrees, commits
internal/githubcli/   `gh` wrapper with a dry-run mode
internal/ingest/      Issue polling and task creation
internal/prompt/      Prompt building and untrusted-content fencing
internal/labels/      The factory:* label vocabulary
```

## What this intentionally is not

No Kubernetes, no Redis, no OAuth or GitHub App, no React dashboard, no
automatic merging, no multi-tenancy. One binary, one SQLite file, one machine.
