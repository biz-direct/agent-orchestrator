# Example pipelines

A ready-to-copy **Build → Test → Review** workflow for AO worker tasks. Nothing here
runs by itself: AO reads these files from a project's repository and the daemon
enforces them.

| File | What it is |
| --- | --- |
| `profiles/tester.yaml` | A test-only specialist: instructions, change scope, independent checks |
| `workflows/build-test-review.yaml` | The default sequence: regular worker, Tester, AO's built-in reviewer |
| `workflows/build-review.yaml` | A shorter sequence without a Tester |
| `instructions/tester.md` | The Tester's role instructions |

## Adopt it

```bash
mkdir -p .ao/pipelines/profiles .ao/pipelines/workflows docs/ai
cp examples/pipelines/profiles/*.yaml .ao/pipelines/profiles/
cp examples/pipelines/workflows/*.yaml .ao/pipelines/workflows/
cp examples/pipelines/instructions/tester.md docs/ai/tester.md
# Edit the validation commands in .ao/pipelines/profiles/tester.yaml for your repo.

ao pipeline validate                       # file/field/message for every problem
ao pipeline ls                             # shows what AO can run
ao pipeline trust                          # authorize AO to run the profile's commands
ao pipeline default set build-test-review  # optional: new tasks start it automatically
```

Definitions are discovered under `.ao/pipelines/` on every request and validated
strictly (unknown fields, ambiguous YAML, symlinks, and paths outside the repo are
errors). A run snapshots the definitions when it starts, so later edits never change
a run in flight.

## Use it

```bash
ao spawn --name add-login --prompt "..." --pipeline build-test-review   # explicit
ao spawn --name tiny-fix --prompt "..." --no-pipeline                    # ordinary worker
ao spawn --name task --prompt "..."                                      # project default
ao pipeline status --session <id>                                        # progress and evidence
```

The desktop New task dialog has the same per-task choice, and the task's Pipeline
section shows each stage, the commit every result covers, validation output, review
and CI readiness, the repair budget, and pause/resume/cancel.

## What to know

- **Trusted commands.** Validation commands are repository-controlled. AO never runs
  them until a user authorizes the project (`ao pipeline trust`).
- **Not a sandbox.** `allowedPaths` is checked on the commits a specialist hands
  off, judged on the net diff from the input revision (an out-of-scope commit that
  is later reverted passes, and stays in the PR history). Stages share one worktree
  and run one at a time; this is not OS isolation.
- **Publishing and merging stay yours.** A pipeline never merges, pushes on your
  behalf, or implies host approvals or branch protection were satisfied.
- **Review needs a pull request.** Without one the Review stage waits with a clear
  reason; it never passes.
- **Budget.** Three automatic returns to Build by default, shared by Test and Review.
  When spent, only a person can authorize more.

See [docs/pipelines.md](../../docs/pipelines.md) for the full reference.
