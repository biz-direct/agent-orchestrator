# ao pipeline

Discover, start, supervise, and control repository-defined pipelines (for example Build, then Test, then Review) on worker tasks. The daemon enforces ordering, scope, validation, review, and repair gates; callers choose and supervise but cannot force a gate.

## Syntax

```
ao pipeline <subcommand> [flags]
```

## Subcommands

| Subcommand | Meaning |
|---|---|
| `ls [--project id] [--json]` | List profiles and workflows (stable ids, descriptions, validity, whether executable) and the project default. Alias: `list`. |
| `validate [--project id]` | Re-read `.ao/pipelines/` and print every diagnostic. |
| `default get\|set\|clear` | Read or change the project default (a reference; users decide). |
| `trust [--project id] [--revoke]` | User only: authorize AO to run validation commands declared in repository files. |
| `start <workflow-id> --session <worker-id>` | Run a workflow on an existing worker. Inside an AO session the workflow defaults apply; stage harness/model overrides are user-only. |
| `status [--session id] [--json]` | Current stage, attempts, commits, validation, review readiness, repair budget, pause reason, and last recovery. |
| `submit --outcome succeeded\|failed\|production_defect [--summary ...] [--report-file f]` | Worker only: submit the active stage's result from a clean committed worktree. `ao report` never completes a stage. |
| `pause\|resume\|cancel [--session id] [--reason ...]` | Control the run. Idempotent; stale requests are refused. Cancel never resets or deletes anything. |
| `resume --restore-conversation` | User only: restore the same conversation after a recovery decision. |
| `authorize-repairs [--count N]` | User only: allow additional automatic repairs once the budget is spent. |

Select a workflow when spawning:

```bash
ao spawn --project app --name "add-login" --prompt "..." --pipeline build-test-review   # explicit workflow
ao spawn --project app --name "tiny-fix" --prompt "..." --no-pipeline                    # ordinary worker
ao spawn --project app --name "task" --prompt "..."                                      # project default, if any
```

## Rules for agents

- Only `executable` workflows can be selected. Do not assume a workflow exists; run `ao pipeline ls --json` first.
- You cannot override a stage's harness or model, wake an inactive stage, force a gate to pass, resume a pause that is a person's decision (recovery, spent repair budget), or authorize extra repairs. Report what is needed and let the human decide.
- A worker saying it is done mid-pipeline is a signal. Treat the task as complete only when AO reports the pipeline completed. Pipeline completion never merges anything.
- Workers submit with `ao pipeline submit`; they must not use `ao report` to advance a stage.
