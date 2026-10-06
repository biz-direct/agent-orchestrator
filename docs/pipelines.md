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
| Test repair through the original worker (shared three-attempt budget) | shipped |
| Built-in Review stage with current-revision approval and CI gates | shipped |
| Review repair through Build back to Review (shared budget) | shipped |
| Pause, resume, cancel, human-authorized extra repairs | shipped |
| Recovery after daemon/desktop replacement | in flight, tracked by the subissues of #1 |

A valid workflow that this build cannot execute can be selected but is shown as
**unavailable**. AO never silently substitutes a normal worker for a selected workflow that cannot run, and a
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

## Repair: Test failures return to the original worker

When a specialist stage declares `repairTo: build`, two outcomes send the task
back instead of pausing: a `production_defect` report, and a *genuine*
validation failure (a required check that exited non-zero). The route is
**Build → the failing stage again**:

1. One atomic change closes the failing attempt, counts the return, and creates a
   Build attempt (in `handoff`) holding revision-bound feedback: the defects or
   failed checks, the revision they are about, and a bounded tail of the log.
2. The failing stage's executor is fenced and proven stopped, the worktree is
   verified to be exactly the tested revision, and the **original worker
   conversation** is resumed with the feedback as a new turn.
3. When Build submits a clean committed checkpoint the run returns to the stage
   that failed and **resumes that stage's own conversation** (attempt 2, 3, …),
   told that earlier results do not cover the new revision.

**Budget.** The run has one shared budget of automatic returns to Build
(`repairBudget`, default 3; the initial Build is not a repair). Each return is
counted exactly once: the repair record is unique per failing attempt and is
written in the same transaction that spends the budget, so duplicate feedback,
restarts, and concurrent transitions cannot spend it twice or grant extras. When
the budget is spent the run pauses (`repair_budget_exhausted`) *before* a fourth
return and stays paused; ordinary resume never grants more attempts. Later
Review feedback uses the same accounting (`planRepair` with kind
`review_feedback`; see "Review repair" below).

**Not repairs.** Operational failures (setup, credentials, launch, timeout,
cancellation, unknown), policy violations (scope, dirty or stale submissions),
and stages with no `repairTo` pause instead and never consume the budget.

**Revision scope.** Specialist evidence names the commit it covers. Once Build
produces a newer checkpoint, earlier evidence is retained but marked as no longer
applying; it never counts as validation of the new head.

**Safe resume.** If a conversation that must be resumed has no running controller
(for example after a daemon restart), AO does **not** start a fresh conversation
in its place: the run pauses with `recovery_decision_required`. Recovery tooling
arrives with the recovery slice. Nothing in this flow resets or rolls back the
worktree.

## Review: AO's built-in reviewer with current-revision gates

A `kind: review` stage is an **adapter** over AO's existing review subsystem, not
a new worker role and not a second reviewer. It uses the task's pull request, the
configured reviewer and its interface (terminal or chat), and the same review
runs, controls, and Review panel as an ordinary task. There is no per-stage
harness or model: a stage override on a review stage is refused; change the
reviewer in the task's review settings.

**Handoff.** After the previous stage is accepted, the writer (the worker or the
Tester) is fenced and proven stopped and the worktree is verified to be exactly
the accepted checkpoint. The Review attempt then becomes `active` with no
executor. It is evaluated from durable facts by the same background driver that
runs handoffs, every few seconds and at every wake, so a daemon restart simply
continues it.

**What it waits for**, in this order, always for the exact checkpoint revision:

1. The workspace is on the expected branch at the checkpoint with no tracked
   edits (untracked files cannot change the head). Anything else pauses
   (`head_changed`, `unexpected_changes`, `review_unverifiable`).
2. The task's own open pull request for the branch. None yet → waits
   (`awaiting_pull_request`); merged/closed → pauses (`pull_request_closed`);
   several → pauses (`pull_request_ambiguous`). A pull request head that has not
   reached the checkpoint waits (`awaiting_pull_request_head`); once the review
   is linked, a head that moves pauses (`head_changed`).
3. An AO review pass **for that head**. With **Auto review on** the run starts
   the built-in review itself, through the same service auto-review uses
   (subject to its eligibility, for example idle time, which only waits). With
   it **off**, the run shows `awaiting_manual_review` and the existing review
   controls stay available. A pass for any other head never counts.
4. The pass's result. Approved continues; `changes_requested` is routed to
   Build when the stage declares `repairTo` (see "Review repair"), otherwise it
   pauses the run (`review_changes_requested`) with the verdict kept as the
   attempt's result, spends no repair budget, and **does not nudge the worker**;
   a failed or cancelled reviewer, or one that ended
   without a verdict, pauses (`review_operational`) and is not retried behind
   the run's back.
5. Required CI for that head. AO has no per-check "required" flag, so it uses the
   provider's merge state: passing → ok; failing or pending with merge state
   `UNSTABLE` → only non-required checks are affected, ok; failing otherwise →
   pauses (`ci_failing`) because AO cannot prove the check is not required;
   pending otherwise waits (`awaiting_ci`); results for another commit, or CI
   that has not been observed, wait (`awaiting_ci_status`). A repository whose
   provider reports **no checks** after CI was observed has no CI requirement and
   AO does not invent one. Limitation: a required check that has not reported
   yet cannot be told apart from "no checks"; the review itself normally takes
   long enough for checks to appear.

Only when approval **and** CI hold for the current head does the run complete.
Completing a pipeline never merges, never publishes, and never implies that
repository-host approvals, branch protection, or explicit merge authorization
were satisfied: those stay independent requirements.

**Exclusivity.** While a run is unfinished, a manual review request is refused
(`409 PIPELINE_REVIEW_NOT_READY`) unless the run is at its Review stage, and
idle-worker automatic review stays suppressed for the task; only the Review stage
starts a pass. Direct review-feedback nudges to the worker are deferred while a
run owns the task. An agent can never approve its own review: `ao pipeline
submit` on a Review attempt is refused (`PIPELINE_STAGE_NOT_SUBMITTABLE`).
Non-pipeline tasks are unchanged.

**Visibility.** `GET /sessions/{id}/pipeline` returns `reviewGate` (state
`waiting`, `ready`, or `blocked`, a machine `code`, the explanation, the
checkpoint and pull request head, the review run and verdict, and the CI
judgement) while the run is at Review, plus `reviews`: each Review attempt's
linked head, review run, and outcome. `ao pipeline status` and the task view
render the same facts. The durable link (`pipeline_review_links`) records the
exact pull request head and review run once the head matches and can never move
to another head.

### Review repair

When the Review stage declares `repairTo: build`, a `changes_requested` verdict on
the **current head** is routed through the run, never through a direct nudge:

1. One atomic change closes the Review attempt (`changes_requested`), counts the
   return against the **same budget Test repairs use**, and creates a Build
   attempt holding revision-bound feedback: the review run, its GitHub review id
   (so the worker can reply to it), and the review body.
2. The original worker conversation is resumed once with that feedback and told
   to commit, push to the task's pull request branch, and submit.
3. When Build submits a clean committed checkpoint the run goes **directly back to
   Review**, not through Test. A new Review attempt evaluates the new head:
   Auto review on starts the built-in review again; off waits for a manual
   trigger. It needs a fresh approval and fresh required CI for the repaired
   revision; the previous verdict and any CI results for the old head never count.
4. Earlier Test evidence stays visible but is labelled as covering an earlier
   revision; it is never presented as validation of the repaired head.

Feedback for any other head is ignored. Duplicate delivery, concurrent drivers,
reconciliation, and daemon restarts cannot spend the budget twice (one repair
record per failing attempt, written in the same transaction). When the budget is
spent the run pauses (`repair_budget_exhausted`) before another return, and a
dead original conversation asks for a recovery decision rather than starting a
fresh one.

While a pipeline run is unfinished, AO's automatic CI, review-comment, review
feedback and merge-conflict nudges to the worker are held (nothing is recorded as
sent, so a nudge that still applies fires once the run has finished); the run
carries review feedback itself.

## Pause, resume, and cancel

`ao pipeline pause|resume|cancel` (and the task view's controls, and
`POST /sessions/{id}/pipeline/control`) operate on the task's current run. Requests
name the run and may carry the revision the caller read; a request for an older
run or a changed revision is refused (`409 PIPELINE_STALE_CONTROL`). Repeating a
request that is already satisfied (pause a paused run, cancel a cancelled one,
resume a running one) succeeds with `changed: false`.

**Pause** persists first, which fences new stage starts and handoffs (the driver
only advances running runs, and a paused run accepts no results), then stops the
active execution:

- executor stages (Build, specialists): the executor's turn is interrupted and AO
  tries to prove it quiescent. A requested interrupt is **not** proof: if a turn,
  background task, or pending permission/input request cannot be shown to be gone,
  the pause is still recorded and the response says `stop.confirmed: false` with
  the reason. AO never answers a permission prompt for you. The executor stays
  reachable so a person can unblock it. Interrupting settles the executor's queued
  turns as cancelled.
- validation: the in-flight round is cancelled and its process groups are killed;
  each command is recorded as `cancelled`.
- Review: nothing executes; evaluation simply stops. AO's built-in reviewer keeps
  its own lifecycle and the ordinary review controls.

These three cases are the stage "hooks": a new stage kind adds its own case where
the pause/cancel stop is dispatched instead of a new control path. Pauses are
operational (`paused_by_user`, `paused_by_orchestrator`) and never spend the repair
budget.

**Resume** revalidates before restarting anything: the task must be live, the
workspace must be on the expected branch, and history must still descend from the
stage's input revision (a Review resume additionally needs HEAD to equal the
accepted checkpoint with no tracked edits, otherwise `409 PIPELINE_RESUME_BLOCKED`
says what to restore). It then creates a **new attempt** that continues the
interrupted one (`retryOfAttemptId`), keeping the stage's input revision: the same
conversation is resumed with a note that its earlier turn is not replayed. If that
conversation's controller is gone, the run pauses for a recovery decision
(`recovery_decision_required`) instead of starting a fresh one. A pending handoff or
validation is simply handed back to the driver (validation runs a new round and
keeps the earlier results). Pauses that are decisions for a person
(`recovery_decision_required`, `repair_budget_exhausted`, `validation_interrupted`)
cannot be resumed by an orchestrator.

**Cancel** ends the run (`cancelled`) and stops active execution the same way. It
never resets the worktree, deletes files, closes the pull request, or removes
conversations: stage conversations are retained (but closed to input), the original
worker keeps the task, and its fenced intake is reopened. A finished run (completed
or cancelled) likewise hands the worker's conversation back. A cancelled run does
not block starting a new one.

**Extra repairs are a separate, human authorization.** When the budget is spent the
run stays paused (`repair_budget_exhausted`) and resume refuses
(`409 PIPELINE_REPAIR_AUTHORIZATION_REQUIRED`); an orchestrator cannot authorize
(`403`). A person runs `ao pipeline authorize-repairs --count N` (or uses the task
view), which persists one grant (`pipeline_repair_grants`, idempotent per request
key) and raises the run's budget in the same transaction; `resume` then applies
exactly one authorized repair from the failing attempt's retained feedback.
