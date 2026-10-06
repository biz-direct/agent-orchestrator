# Repository-defined pipelines

Specialist profiles and sequential workflows are **defined in repository files**.
AO discovers, validates, selects, executes, and monitors them. There is no visual
definition editor and no global profile-inheritance library in v1.

Status by delivery slice (parent design: issue #1):

| Capability | State |
| --- | --- |
| Discover/validate definitions, project-default selection | shipped |
| Single-stage Build run on an existing Chat worker, snapshots, verified results | shipped |
| Attached Chat specialists run sequentially in the same worktree | shipped |
| Specialist scope enforcement and structured result contract | shipped |
| Independent validation commands with revision-bound evidence | shipped |
| Review, repair, pause/resume, recovery | in flight, tracked by the subissues of #1 |

A valid workflow that this build cannot execute (one with a `review` stage today) can be selected but is shown as **unavailable**. AO never silently
substitutes a normal worker for a selected workflow that cannot run, and a
project default never changes what an ordinary spawn launches.

## Where definitions live

Read from the project's repository root on every request:

```
.ao/pipelines/profiles/<name>.yaml     one specialist profile per file
.ao/pipelines/workflows/<name>.yaml    one sequential workflow per file
```

Only `.yaml`/`.yml` files are read. Symlinked definition or instruction files,
paths that resolve outside the project folder, files over 256 KiB, and
multi-document, anchor/alias, or merge-key YAML are rejected as ambiguous.
Unknown fields are errors, not ignored.

## Profile (`version: 1`)

```yaml
version: 1
id: tester                          # 1-64 chars: a-z 0-9 -
description: Writes and improves tests
# Exactly one of:
instructions: |
  You are the Tester. Only change tests.
# instructionsFile: docs/ai/tester.md   # repo-relative, regular file, <= 64 KiB
harness: claude-code                # optional default; explicit user choices win
model: opus                         # optional default
allowedPaths:                       # change scope (handoff constraint, not a sandbox)
  - "**/*_test.go"
  - "test/**"
validation:                         # run independently by AO at the checkpoint
  - id: unit
    command: go test ./...
    timeoutSeconds: 600             # default 600, max 3600
    required: true                  # default true
```

`allowedPaths` globs are repo-relative, use `/`, may use a whole-segment `**`,
and must not be absolute, contain `..`, be negated (`!`), or target `.git`.
Path validation constrains what a stage may hand off; it is **not** a filesystem
or process sandbox.

## Workflow (`version: 1`)

```yaml
version: 1
id: build-test-review
description: Build, then Test, then Review
repairBudget: 3          # shared automatic returns to Build, 0-10, default 3
stages:
  - id: build
    kind: build          # the regular worker: owns task, worktree, branch, PR
  - id: test
    kind: specialist     # attached Chat specialist
    profile: tester
    repairTo: build      # optional; omitted means a failure pauses for a human
  - id: review
    kind: review         # AO's built-in reviewer
    repairTo: build
```

Rules: stages are strictly sequential; exactly one `build` stage and it is
first; `review` (at most one) is last; `specialist` stages require a valid
`profile`; `repairTo` must name an earlier `build` stage. Graph features
(`parallel`, `dependsOn`, `next`, `branches`, `when`, ...) and `terminal`
stages are rejected with an explanatory error.

## Inspect and select

```bash
ao pipeline ls                      # profiles, workflows, status, current default
ao pipeline validate                # exits 1 and prints file/field/message per problem
ao pipeline default set <workflow>  # save the project default
ao pipeline default set --normal-worker   # explicit normal-worker choice
ao pipeline default clear
```

Daemon routes: `GET /api/v1/projects/{id}/pipelines`,
`GET|PUT /api/v1/projects/{id}/pipelines/default`. The default is stored as a
reference in the project config (`defaultPipeline`) by a focused write that
leaves every other project setting untouched. The desktop shows the same
catalog under **Settings → Project → Pipeline**.

## Running a single-stage Build workflow

A workflow whose only stage is `build` runs on an **existing Chat worker task**.
The worker stays the owner of the worktree, branch, and PR; the run only records
progress and decides whether a result may advance it.

```bash
ao pipeline start <workflow-id> [--session <id>]   # snapshots the definition and messages the worker
ao pipeline status                                  # stages, attempts, checkpoint, last rejection
ao pipeline submit --outcome succeeded --summary "…"   # run by the worker when committed
```

- **Snapshot.** Starting a run freezes the workflow, every referenced profile, the
  resolved instruction contents, validation configuration, and each stage's
  harness/model (with the hash of every definition file). Later repository edits
  affect future runs only.
- **Overrides.** Only an explicit *user* may override a stage's harness/model
  (`--override-stage`, `--harness`, `--model`). A command run inside an AO session
  is treated as an orchestrator and cannot. Instructions, path constraints, and
  gates apply either way.
- **Verified results.** `ao pipeline submit` only *claims* a result. The daemon
  accepts it for the active attempt, under the controller generation the attempt
  started with, when the worktree is clean, on the expected branch, and `HEAD`
  equals the reported commit and descends from the stage's input commit. A
  no-change stage keeps its input commit. Dirty or stale submissions are rejected
  with the offending paths; AO never resets, cleans, or amends the worktree.
  Submissions are idempotent per `idempotencyKey`.
- **`ao report` is informational.** A done report never completes a stage.
- **While a run is unfinished** (`running` or `paused`), automatic review and
  merge-driven completion/cleanup stay out of the way. Ordinary workers are
  unaffected.
- **Interruption.** If the worker's controller restarts or the task ends mid-stage,
  the run pauses (it is never replayed) and does not consume the repair budget.
  Resume/cancel controls arrive with the pause/resume slice.

Daemon routes: `GET|POST /api/v1/sessions/{id}/pipeline`,
`POST /api/v1/sessions/{id}/pipeline/results`. Progress surfaces through the
existing session change stream; the desktop shows it in the task inspector
(**Summary → Pipeline**).

## Attached specialists (Build → Test → …)

A `specialist` stage runs in its **own Chat conversation** inside the worker's own
worktree. It is a hidden session row attached to the worker: it never appears as
a board task, never owns the workspace, branch, or PR, and is excluded from every
session listing, so the reaper, SCM observer, and startup reconcile cannot see or
restart it. Only Chat controllers are supported; an unsupported harness is
refused at start (or pauses the handoff) and **never falls back to a terminal**.

Execution is exclusive. After a stage's result is accepted, the successor is
recorded in `handoff` and a background driver (never the submitting request,
which would deadlock on its own turn):

1. fences the source executor's intake and drains it, then verifies no running
   turn, queued work, pending approval, or background activity (idle alone is not
   proof);
2. verifies the worktree is still exactly the accepted checkpoint: same branch,
   clean, `HEAD` equal to the accepted output commit;
3. starts the specialist with its profile instructions, the task, the input
   revision, and the handoff summary (it does not inherit the previous
   executor's reasoning);
4. only then marks the successor `active`.

Anything unprovable pauses the run (`handoff_uncertain`, `unexpected_changes`,
`stage_unsupported`, `stage_start_failed`) with the successor still recorded.
Operational pauses never consume the repair budget and never reset the worktree.

While a run is unfinished, an **execution gate** admits only the active stage's
executor: messages, nudges, steering, approvals, restores, and resumes for the
worker (during a specialist stage), for a finished specialist, or for anyone
during a handoff are refused with `PIPELINE_EXECUTION_OWNED`. Ordinary sessions
are never affected.

## Specialist scope and result contract

A specialist's result is a **claim the daemon verifies**, never a bare "done".

```bash
ao pipeline submit --outcome succeeded --summary "added tests" --report-file report.json
```

`report.json` (written outside the worktree) has this shape; a passing report needs
at least one finding and none `unmet`, and a `production_defect` report must describe
at least one defect:

```json
{
  "findings": [{"criterion": "…", "status": "met|unmet|not_applicable|unverified", "evidence": "…"}],
  "commands": [{"command": "go test ./...", "exitCode": 0, "summary": "ok"}],
  "remainingIssues": ["…"],
  "defects": [{"description": "…", "paths": ["src/main.go"]}]
}
```

Outcomes: `succeeded` (advances), `production_defect` (a bug in production code,
reported for return to Build and never fixed under specialist authority; until
repair routing lands the run pauses with the retained report), `failed`.

**Scope.** Before accepting, the daemon lists the stage's *entire* diff from the
accepted input checkpoint (every commit since, with renames split into
delete + add) and checks each path against the profile's snapshotted
`allowedPaths`. A profile that lists no `allowedPaths` may change nothing.
Out-of-scope paths, symlinks that point outside scope or the repository, absolute
symlinks, and submodule changes are refused with the offending paths. The
commits and files are **preserved** (never reset or amended); the stage stays
active and can advance once the *net* change is back in scope (for example by
committing a revert). Path validation is a hand-off constraint, not a filesystem
or process sandbox: it cannot stop a process writing elsewhere, only refuse to
advance a stage whose commits do.

Specialist results are listed as **evidence bound to the exact revision they
cover**; they are never presented as validation of a later head.

## Independent validation

A profile may declare `validation` commands. For a specialist's **passing** result
the agent's report is only half the gate: after the daemon verifies the commit
and scope it moves the attempt to `validating`, proves the executor stopped, and
**runs the snapshotted commands itself** against the exact committed checkpoint.
The stage advances only when the structured outcome *and* every `required` check
agree.

- **Trusted execution.** Commands come from repository files, so AO runs them
  only after the user authorizes it: **Settings → Project → Pipeline → Run
  repository validation commands**, or `ao pipeline trust` / `--revoke`. The flag
  is never read from a repository file and is checked every time commands are
  about to run, so revoking it stops later commands. Without it the run pauses
  with `commands_not_authorized` and nothing runs.
- **Snapshot.** The command list and timeouts come from the run's startup
  snapshot; neither the agent nor a later repository edit can replace them for
  an active run.
- **Exclusive.** While `validating`, the execution gate refuses every session, and
  one driver slot per run collapses overlapping requests into one.
- **Evidence.** Each command is recorded *before* it starts (so a crash leaves
  proof it may have run) and then with its identity, revision, timing, exit
  status, and a bounded log (first 8 KiB + last 24 KiB). Logs are sanitized
  (terminal escapes and control bytes removed; token-shaped strings and the
  values of secret-named environment variables redacted). Commands run with the
  daemon's environment minus every `AO_*` variable.
- **Verdicts.** A clean exit passes; an ordinary non-zero exit of a `required`
  check is a genuine failure (`validation_failed`, retained for repair routing).
  Launch failures (exit 126/127, cannot start), timeouts, cancellation, and
  unknown results are **operational**: the run pauses (`validation_operational`)
  and they are never read as code defects or spend the repair budget.
- **Cleanup.** Every command runs in its own process group with an explicit
  timeout; on timeout or cancellation the whole group is killed, so nothing
  outlives its round (on Windows only the shell process is killed).
- **After the checks.** Branch, `HEAD`, and tracked files must be unchanged
  (untracked build output is tolerated). Anything else pauses
  (`validation_mutated_workspace`) with the changes preserved.
- **Restart.** A command still marked running when AO restarts is recorded as
  `unknown`, the run pauses (`validation_interrupted`), and it is neither assumed
  to have passed nor retried.
