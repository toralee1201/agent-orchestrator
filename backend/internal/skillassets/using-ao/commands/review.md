# ao review

Manage Orchestrator's native code reviewer for a worker's PR.

Orchestrator's reviewer is an isolated reviewer agent for implementation PRs owned by a
worker. To review that worker's PR, use `ao review trigger`. Inside a worker,
the review commands default to the calling session (`AO_SESSION_ID`), so
`ao review trigger` reviews your own implementation PR. An orchestrator passes
the implementation worker's session id.

For a requested review of someone else's PR, spawn or use a normal review-only
worker without claiming or attaching the target PR. That worker submits the
requested provider review and comments directly. Do not run `ao review trigger`,
create an ownership-only placeholder, or require an implementation owner just
to review the PR.

Orchestrator's review is internal. An Orchestrator approval is not a GitHub approval: it does not
satisfy required or independent-account reviews or branch protection, and it
does not authorize merging.

## Syntax

```
ao review <subcommand> [args] [flags]
```

The review loop can be inspected, submitted, cancelled, or triggered again.

## Subcommands

---

### ao review ls

List review runs for a worker session (default: the calling session). Alias: `list`.

**Syntax:**
```
ao review ls [worker-session-id] [flags]
```

**Flags:**

| Flag | Meaning | Default / Required |
|---|---|---|
| `--json` | Output reviews as JSON | - |

**Example:**

```bash
ao review ls mer-3
```

---

### ao review submit

Record a reviewer's result for a worker's PR.

**Syntax:**
```
ao review submit [worker-session-id] [flags]
```

**Flags:**

| Flag | Meaning | Default / Required |
|---|---|---|
| `--body string` | Review body: a path to a Markdown file, or `-` to read from stdin | - |
| `--review-id string` | Id of the GitHub PR review just posted (the `.id` from the `gh api` POST that created the review) | - |
| `--reviews string` | JSON review results array or object: a path, or `-` to read from stdin | - |
| `--run string` | Review run id | Required |
| `--session string` | Worker session id (or pass it as the positional argument) | - |
| `--verdict string` | Review verdict: `approved` or `changes_requested` | Required |

If the local daemon is restarting when a result is submitted, Orchestrator retains the
parsed result in memory and retries the same idempotent request for up to 30
seconds. Validation errors return immediately. If the daemon remains unavailable,
the command reports failure and can be repeated safely; daemon idempotency
handles the case where an earlier connection dropped after committing the result.

**Examples:**

```bash
# Submit an approved review for session mer-3
ao review submit mer-3 --run review-run-1 --verdict approved
```

```bash
# Submit a changes-requested review with a body from stdin
echo "Please fix the null check on line 42." | ao review submit --session mer-3 --run review-run-1 --verdict changes_requested --body -
```

---

### ao review cancel

Cancel every running review for a worker's PR (default: the calling session). Alias: `stop`.

**Syntax:**
```
ao review cancel [worker-session-id] [flags]
```

**Flags:**

| Flag | Meaning | Default / Required |
|---|---|---|
| `--session string` | Worker session id (or pass it positionally) | - |

**Example:**

```bash
ao review cancel mer-3
```

---

### ao review trigger

Start Orchestrator's reviewer on a worker's open PR heads (default: the calling session).
Aliases: `execute`, `restart`. Reviewer panes cannot run it.

**Syntax:**
```
ao review trigger [worker-session-id] [flags]
```

**Flags:**

| Flag | Meaning | Default / Required |
|---|---|---|
| `--session string` | Worker session id (or pass it positionally) | The calling Orchestrator-managed session |
| `--pr string` | Review only this session's implementation PR URL, attaching it to the session first if Orchestrator does not track it yet (never from another active session). Do not pass an external review target | Every eligible PR on the session |
| `--agent string` | Reviewer agent for this pass only (alias `--harness`) | Session reviewer, then project reviewer, then the project's default worker agent |
| `--model string` | Reviewer model for this pass only | As above |
| `--effort string` | Reviewer reasoning effort for this pass only | As above |
| `--rerun` | Review a head that already has a review again, or add a reviewer with a different agent alongside one that is still running | - |
| `--no-inject` | Leave the session's review auto-inject setting unchanged | Auto-inject is turned on |

Orchestrator fetches the session's PRs fresh from the provider before deciding what is
due, so the pass covers the commit really on the PR. Right after opening a PR,
pass `--pr <url>`.

A head that is already being reviewed, or already has a review, is not reviewed
again: the command exits 1 with `REVIEW_ALREADY_RUNNING` or
`REVIEW_HEAD_ALREADY_REVIEWED` and says what to do. Push new commits, or pass
`--rerun`. The trigger re-reads the PR from the provider first; if you
just pushed and it still says already reviewed, wait a few seconds and retry. The same reviewer agent never runs twice on one head at the same time.

Record the head's unit-test pass with `ao review tests-passed --sha <head>`
before triggering. The daemon runs the project's unit-test command and records
the pass only on exit 0, so a failing suite leaves the head untriggerable. A
trigger for a head with no recorded pass is refused.

By default the command turns on the worker session's review auto-inject. The
reviewer posts an inline GitHub comment for each change it requires, and Orchestrator
forwards those comments to the worker like any other review comments. An
approval adds no comments. Orchestrator writes the worker and reviewer done
reports itself; do not run `ao report --done` for that verdict. If a run
shows `failed` or `cancelled`, trigger again.

**Examples:**

```bash
# Inside a worker: request a review of this session's PR once it is ready
ao review trigger
```

```bash
# Orchestrator: review a worker's PR with a specific reviewer
ao review trigger mer-3 --agent codex --model gpt-5.5
```

```bash
# Add a second opinion on the same commit while the first reviewer runs
ao review trigger --agent claude-code --rerun
```

---

### ao review tests-passed

Run the project's `unitTestCommand` in this worker's worktree and record the
pass for one PR head. `AO_SESSION_ID` is required.

The daemon, not the worker, decides the outcome: it runs the command in the
worker's worktree, and records the pass only when the command exits 0. A failing
suite records nothing and the command fails with the tail of the output, so fix
the tests and rerun. The worktree must still be on `--sha`, because a pass for a
head the worktree is not on would let a trigger review an untested commit. A
project with no `unitTestCommand` refuses the call; set one with
`ao project set-config <id> --unit-test-command "<command>"`.

`ao review trigger` refuses a head that has no record.

**Syntax:**
```
ao review tests-passed --sha <head>
```

**Flags:**

| Flag | Meaning | Default / Required |
|---|---|---|
| `--sha string` | PR head commit to test; the pass is recorded only when its tests exit 0 | Required |

**Example:**

```bash
ao review tests-passed --sha abc123def
```
