# Rebuilding this yourself

The point of this repository is that you can throw it away and write your own.
This is the order to do that in, and the trap at each step.

Rule for every milestone: **finish with one test and one `curl` you can run.**
If you cannot demonstrate a milestone from a terminal, it is not done.

## Read this first, in this order

1. `README.md` — what the system does
2. `docs/ARCHITECTURE.md` — why each piece exists
3. `internal/store/tasks.go` — the task lifecycle is the whole design
4. `internal/worker/worker.go` — `execute()` is the pipeline in one function

Everything else is detail.

---

## Milestone 1 — Tasks in SQLite

**Build:** a `tasks` table, `CreateTask`, `GetTask`, `ListTasks`, and
`POST /tasks` + `GET /tasks` + `GET /tasks/{id}`.

Skip kinds, GitHub fields, and workers. A task is an ID, a title, a prompt, and
a status.

**Trap:** reaching for an ORM or a migration framework. Write the SQL. There
are five tables in the finished system and you want to be able to read all of
them.

**Prove it:**

```bash
curl -s -X POST localhost:7337/tasks -d '{"repo_owner":"local","repo_name":"demo","title":"hello","prompt":"hi"}' | jq
```

**Test:** create → list → get returns what you stored.

---

## Milestone 2 — Task events

**Build:** a `task_events` table and `GET /tasks/{id}/events`. Append an event
on creation.

Do this *before* workers. Once workers exist you will desperately want to see
what they did, and retrofitting logging is miserable.

**Trap:** logging to stdout only. The log has to live in the database, because
the process that produced it will have exited by the time you look.

**Prove it:** `curl localhost:7337/tasks/<id>/events`

---

## Milestone 3 — Workers and claiming

**Build:** a `workers` table, `POST /workers/register`, `POST /workers/heartbeat`,
and `POST /tasks/claim`. Claim the oldest queued task and mark it running.

Then write the smallest possible worker: register, poll claim, log the task,
complete it. No git, no runtime.

**Traps:**
- Returning 404 or an error for an empty queue. Idle is normal — return **204**.
- Generating a new worker ID on every start. Persist it to a file.
- `SELECT` then `UPDATE` without a transaction. Two workers will claim the same
  task. Do it in one transaction and check `RowsAffected`.

**Test:** claim on an empty queue returns "no task"; claim after creating one
returns that task; a second claim returns nothing.

---

## Milestone 4 — Leases

This is the milestone that teaches the most. Do not skip it.

**Build:** `lease_token`, `lease_expires_at` and `attempt_count` on tasks.
`ClaimTask` stamps a random token. `POST /tasks/{id}/complete` and
`POST /tasks/{id}/events` **require** that token. A background ticker requeues
expired leases, and gives up after N attempts by marking the task `lost`.

**Traps:**
- Trusting the worker to say "I died". It cannot. Expiry has to be the server's
  job.
- Making the clock untestable. Inject it (`SetClock`) so a test can jump two
  minutes forward instead of sleeping.
- Requeuing forever. A task that crashes its worker three times will crash the
  fourth. Cap it.
- Requeuing *instantly*. A rate limit or a network blip burns all three
  attempts in five seconds and gives up, when waiting 30s would have worked.
  Add a `run_after` column, set it when requeuing, and filter on it in the
  claim query. Back off exponentially and cap it.
- **Shipping expiry without renewal.** This one bit this repository. If the
  worker never renews, the reaper cannot tell a slow agent from a dead one, so
  any task outliving its lease is requeued and run a *second time* while the
  first agent is still editing files. Add `POST /tasks/{id}/renew` in the same
  milestone as expiry, and have the worker cancel its runtime when a renewal
  comes back 409.
- **Retrying onto the same branch and directory.** A retry reuses the task ID.
  Scope the branch and worktree by attempt number, or attempt 2 dies in
  `git worktree add` before the agent runs and the task is stuck forever.

**Test:** completing with the wrong token fails; an expired lease returns to
`queued`; after `MaxAttempts` it becomes `lost`; a renewed task is *never*
reaped; and a retry after an abandoned attempt runs to completion.

**Prove it:** start a worker, `Ctrl-C` it mid-task, and watch the server log
`lease expired: task ... -> queued`.

---

## Milestone 5 — The fake runtime

**Build:** a `Runtime` interface with one method, and one implementation that
writes a file into a directory. Have the worker call it.

**Trap:** wiring up a real agent CLI now. You will spend a day on someone
else's flags and learn nothing about your own system. The fake runtime is the
thing that makes every later milestone testable — it stays useful forever.

Make the fake runtime write **real files**, not log lines. You want a real diff
at the next milestone.

**Test:** run the runtime against a temp directory and assert the file exists.

---

## Milestone 6 — Git worktrees

**Build:** clone the repository into a cache directory once, then per task:

```
git worktree add -b factory/task-<id> <dir> origin/main
```

Write the prompt to `.factory-task.md` inside it, run the runtime there, and
`git status` afterwards.

**Traps:**
- Cloning per task. Use one cache clone and N worktrees — it is far faster and
  is the reason concurrent agents are practical.
- Letting the prompt file count as a change. `git status` will report
  `.factory-task.md`, so every task will look like it did work. Exclude the
  factory's own files with a pathspec (`:(exclude).factory-task.md`) — see
  `internal/gitx/gitx.go`.
- Deleting worktrees on completion. Keep them. Reading what the agent actually
  wrote is how you debug all of this.
- Making fetch failures fatal. Warn and use the cache; you will develop offline.

**Test:** the end-to-end one. Create a real git repo in a temp dir, run the
worker against it, and assert the worktree is on its own branch, contains the
runtime's file, and that the source repo is untouched.

At this point you have a working system. Everything after is GitHub.

---

## Milestone 7 — Reading GitHub

**Build:** a `gh` wrapper with `ListIssuesByLabel` and `ViewIssue`, a poller
that turns `factory:inbox` issues into `refine_ticket` tasks, and
`POST /github/poll`.

**Traps:**
- Skipping dedupe "for now". Poll twice and you have two tasks; poll on a timer
  and you have hundreds. Add the unique `dedupe_key` in the same commit as the
  poller, and insert the observation and the task in **one transaction**.
- Keying dedupe on the issue alone. Key it on issue **plus workflow**, or an
  issue can never be both refined and implemented.
- Putting the issue body straight into the prompt. Fence it and say it is data.
  Do this the first time, not later.

**Test:** a fake `IssueLister` returning one issue; poll five times; assert
exactly one task exists.

---

## Milestone 8 — Writing to GitHub

**Build:** comment, label, and draft-PR calls, plus a dry-run mode. Have the
worker publish results after the work is done.

**Traps:**
- Acting on the poll-time snapshot. Re-fetch the issue and re-check the label
  immediately before mutating. Humans change their minds between poll and run.
- Building dry-run last. Build it first; it is how you test everything else
  without spamming a real repository.
- Creating non-draft PRs. Draft always.

**Test:** hard to test without mocking `gh`. Test the decision logic — "should
this task act on this issue?" — as a pure function, and exercise the rest by
hand in dry-run mode.

---

## Milestone 9 — A real agent

Only now. Swap the fake runtime for a command template with `{{prompt_file}}`,
`{{worktree}}` and `{{task_id}}` substitution.

**Traps:**
- Hard-coding an agent's flags. They change. Make it a template with an
  override flag.
- Discovering a missing binary halfway through a task. Check `exec.LookPath` at
  startup and fail with a message that names the fix.
- Buffering output until the process exits. Stream stdout and stderr into the
  task log line by line, or a ten-minute agent run looks like a hang.

---

---

## Milestone 10 — Routing, and a second opinion

Two changes turn the single pool of workers into a pipeline.

**Build:** a `kinds` column on `workers`, a kind filter in `ClaimTask`, and a
`--kinds` flag. Empty means "all kinds" so the single-worker setup keeps
working. Then add the `review_pr` stage: poll `factory:review`, find the PR,
branch the worktree from the **PR head** rather than the default branch, fetch
the diff, and post the review as a comment.

**Traps:**
- Filtering in the worker instead of the query. If the worker claims a task and
  then rejects it, the task has already been leased and its attempt count
  burned. Filter in SQL.
- Trusting the claim request alone. Fall back to the kinds recorded at
  registration, so a worker that forgets to ask is still routed correctly.
- Posting a formal GitHub *review* rather than a comment. An approval can
  satisfy branch protection; an agent must never be able to do that.
- Parsing the verdict loosely. `ParseVerdict` must fail closed — an
  unparseable review is a `COMMENT`, never an `APPROVE` — and it should only
  read inside the verdict section, or the word "approve" in prose flips it.
- Letting a fake reviewer approve. Mechanical checks that rubber-stamp a PR are
  worse than no review at all.
- Reusing the implement stage's worktree logic unchanged. A review works on the
  PR's branch; branching from `main` reviews the wrong code.

**Test:** queue an implement task *before* a refine task, then check that a
refine-only worker takes the newer refine task. That ordering is the whole
point — FIFO alone would hand it the wrong one.

## If you want to go further

- **Lease renewal.** A worker heartbeating its lease mid-task, so long runs do
  not need a long fixed lease.
- **Re-review after new commits.** Dedupe is per issue and workflow, so an issue
  gets one review. Include the PR head SHA in the key to review each push.
- **Concurrency per worker.** One task at a time is a deliberate simplification.
- **Worktree pruning.** Every task and retry leaves a checkout on disk. A
  `prune --older-than 7d` that removes the directories and runs
  `git worktree prune` is a short job.
- **Auth.** The API trusts every caller, which is why it binds to loopback and
  is reached over a tunnel. If you ever want it genuinely exposed, that needs
  auth *and* TLS — not a token over plain HTTP.
- **A real dashboard.** `GET /tasks` and `GET /tasks/{id}/events` are already
  enough to build one.

## The three ideas worth taking away

1. **A lease turns "did the worker die?" into "has the clock passed?"** No
   supervisor, no distributed lock, no heartbeat timeout logic in the queue.
2. **A unique dedupe key turns at-least-once polling into exactly-once
   effects.** This is what makes a dumb polling loop safe.
3. **A fake runtime that writes real files makes the whole system testable.**
   It is not a mock to be replaced — it is permanent infrastructure.
