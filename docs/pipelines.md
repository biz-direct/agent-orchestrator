# Repository-defined pipelines

Specialist profiles and sequential workflows are **defined in repository files**.
AO discovers, validates, selects, executes, and monitors them. There is no visual
definition editor and no global profile-inheritance library in v1.

Status by delivery slice (parent design: issue #1):

| Capability | State |
| --- | --- |
| Discover/validate definitions, project-default selection | shipped |
| Execution (Build, attached specialists, validation, Review, repair, recovery) | in flight, tracked by the subissues of #1 |

Until execution ships, a valid workflow can be selected but is shown as
**unavailable**. Tasks still start as ordinary workers; AO never silently
substitutes a normal worker for a selected workflow that cannot run, and the
selection never changes what an ordinary spawn launches.

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
