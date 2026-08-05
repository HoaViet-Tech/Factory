# Demo

Two demos. The first needs **no GitHub account and no network**. Do that one
first — it exercises the entire pipeline.

---

## Demo 1: the full pipeline, no credentials

### 0. Build

```bash
go build -o codefactory ./cmd/codefactory
```

### 1. Make a repository to work on

Any git repo with at least one commit will do. To create a throwaway one:

```bash
mkdir -p /tmp/demo-repo && cd /tmp/demo-repo && git init -b main && echo "# demo" > README.md && git add -A && git commit -m "initial commit"
```

### 2. Start the control plane

```bash
./codefactory server --db ./factory.db --listen 127.0.0.1:7337
```

```
[server] database: ./factory.db
[server] GitHub ingest disabled: gh CLI unavailable: not authenticated...
[server] listening on http://127.0.0.1:7337 (dry-run=false)
```

The GitHub line is expected and harmless — this demo does not use GitHub.

### 3. Register the repository

In a second terminal:

```bash
./codefactory repo add local/demo --clone-url /tmp/demo-repo
```

```
registered local/demo
  clone url:  /tmp/demo-repo
  cache path: <work-dir>/repos/local/demo
  enabled:    true
```

The clone URL is a **local path**, which is exactly why this works offline.

### 4. Queue some work

```bash
./codefactory task create --repo local/demo --title "Add a release checklist" --prompt "The repo should gain a short release checklist. It should list the steps before tagging a release."
```

```
created task 4d2e511e071d (manual) for local/demo
watch it with: codefactory task show 4d2e511e071d
```

Queue a refine task too, to see the ambiguity path:

```bash
./codefactory task create --repo local/demo --kind refine_ticket --title "Refine: make search better" --prompt "Title: make search better

idk it feels slow"
```

### 5. Run a worker

```bash
./codefactory worker --server http://127.0.0.1:7337 --name local-fake --runtime fake --once
```

```
[worker] registered as d1ef7ff85e07fa98 (name=local-fake runtime=fake)
[worker] claimed task 4d2e511e071d (manual) "Add a release checklist"
[worker] [4d2e511e071d] preparing repo cache at .factory/repos/local/demo
[worker] [4d2e511e071d] git clone /tmp/demo-repo .factory/repos/local/demo
[worker] [4d2e511e071d] creating worktree .factory/worktrees/4d2e511e071d on branch factory/task-4d2e511e071d (from origin/main)
[worker] [4d2e511e071d] wrote prompt to .factory-task.md (98 bytes)
[worker] [4d2e511e071d] fake runtime: wrote factory-output/4d2e511e071d.md
[worker] [4d2e511e071d] git status:
?? factory-output/
[worker] [4d2e511e071d] worktree left at .factory/worktrees/4d2e511e071d for inspection
[worker] task 4d2e511e071d -> succeeded
```

`--once` exits after one task. Drop it to keep the worker running, or run it
again to pick up the refine task:

```bash
./codefactory worker --server http://127.0.0.1:7337 --name local-fake --runtime fake --once
```

```
[worker] claimed task 31bdd49bed4a (refine_ticket) "Refine: make search better"
[worker] [31bdd49bed4a] fake runtime: refining "make search better"
[worker] [31bdd49bed4a] fake runtime: ambiguity check -> needs_human=true issue body is under 40 characters, so there is nothing concrete to implement
[worker] [31bdd49bed4a] fake runtime: wrote .factory-refined.md (1063 bytes)
[worker] task 31bdd49bed4a -> succeeded
```

The refiner recognised "idk it feels slow" as too vague. On a real GitHub
issue that verdict becomes the `factory:needs-human` label.

### 6. Look at what happened

```bash
./codefactory task list
```

```
ID            KIND           STATUS     REPO        ISSUE  AGE  TITLE
31bdd49bed4a  refine_ticket  succeeded  local/demo  -      14s  Refine: make search better
4d2e511e071d  manual         succeeded  local/demo  -      14s  Add a release checklist
```

The full log of one task, including every git command:

```bash
./codefactory task show 4d2e511e071d
```

### 7. Inspect the isolated worktree

This is the part worth staring at:

```bash
cd .factory/worktrees/4d2e511e071d && git status && git branch --show-current
```

The task ran on its own branch, in its own directory, and `/tmp/demo-repo` was
never touched. The refined ticket from the other task is at
`.factory/worktrees/<refine-task-id>/.factory-refined.md`:

```markdown
## Goal

make search better

## Background

Reported on local/demo:

> idk it feels slow

## Scope
...
## Risk Notes

BLOCKED: issue body is under 40 characters, so there is nothing concrete to implement.
A human needs to clarify this before an agent should touch the code.
```

### 8. Clean up

```bash
rm -rf .factory factory.db
```

Worktrees are left on disk deliberately so you can inspect them. If you delete
the directories by hand, run `git worktree prune` in the cache clone.

---

## Demo 2: the GitHub flow

Needs `gh` installed and authenticated (`gh auth login`), and a repository you
can write to.

**Start with `--github-dry-run` on both processes.** Reads happen, writes are
logged instead of performed.

> `--github-dry-run` is not an offline mode. Reading issues is a real API call,
> so it still needs an authenticated `gh`. Demo 1 above is the credential-free
> path.

### 1. Create the labels

```bash
./codefactory github labels --repo owner/name
```

### 2. Label an issue

Put `factory:inbox` on a real issue in that repository.

### 3. Start the server and register the repo

```bash
./codefactory server --db ./factory.db --listen 127.0.0.1:7337 --github-dry-run
```

```bash
./codefactory repo add owner/name
```

### 4. Poll

```bash
./codefactory github poll --server http://127.0.0.1:7337
```

```
polled 1 repository
  issues seen:    1
  tasks created:  1
  already known:  0
  (server is in GitHub dry-run mode: no writes will happen)
  created task a1b2c3d4e5f6
```

Poll again — `tasks created` stays 0 and `already known` becomes 1. That is
the dedupe key doing its job.

### 5. Run a worker against it

```bash
./codefactory worker --server http://127.0.0.1:7337 --name gh-fake --runtime fake --github-dry-run
```

The worker re-reads the issue live, checks `factory:inbox` is still there, and
logs the comment and label changes it *would* make:

```
[worker] [dry-run] would comment on owner/name#7 (1204 bytes)
[worker] [dry-run] would edit labels on owner/name#7 (+[factory:ready] -[factory:inbox factory:refining])
```

### 6. Go live

Drop `--github-dry-run` and repeat. The refiner now really comments and
relabels. Once an issue carries `factory:ready`, the next poll creates an
`implement_ticket` task.

To have the implementer push a branch and open a **draft** PR:

```bash
./codefactory worker --server http://127.0.0.1:7337 --name gh-agent --runtime fake --push
```

Without `--push` the branch stays local and the issue is labelled
`factory:review` with a comment explaining where the work is.

### 7. Use a real agent

Swap the runtime once the plumbing is proven:

```bash
./codefactory worker --server http://127.0.0.1:7337 --name gh-claude --runtime claude --push
```

If the CLI's flags differ from the built-in template, override it:

```bash
./codefactory worker --server http://127.0.0.1:7337 --name gh-claude --runtime claude --runtime-command "claude --print --permission-mode acceptEdits" --runtime-stdin
```

---

## Things worth trying

**Kill a worker mid-task.** Start a slow shell runtime, `Ctrl-C` the worker,
and watch the server reap the lease and requeue the task:

```
[server] lease expired: task a1b2c3d4e5f6 -> queued
```

Then run a worker again: the retry gets `attempt-2` paths, and the abandoned
`attempt-1` worktree is still there to inspect.

**Watch a long task keep its lease.** Run something slower than the lease:

```bash
./codefactory worker --runtime shell --runtime-command "sleep 400" --lease-seconds 60
```

The worker renews every 20s and the task is never reaped. Before renewal
existed, this task would have been requeued and run twice concurrently.

**Run two workers at once.** Each gets its own worktree; neither sees the
other's files.

**Remove a label between polling and execution.** The worker re-reads the
issue, notices, and completes without doing anything — the log says exactly
why.
