# Fork decisions

Ledger of how `biz-direct/agent-orchestrator` diverges from upstream
(`OrchestratorInc/agent-orchestrator`). Upstream syncs merge everything; this file
records what the fork then opts out of, so the next sync does not rediscover it.
The workflow is described in [docs/fork-ci.md](docs/fork-ci.md#upstream-sync).

## How to maintain this file

One row per decision, newest first inside each section. Edit it in the same PR that
implements the decision.

| column | meaning |
|---|---|
| Upstream ref | Upstream PR number (`#6478`) or short commit SHA. `-` for a fork-originated deviation. A fork PR is written `fork#59`. |
| Area | Short subsystem name (`telemetry`, `release`, `updater`, `billing`). |
| Decision | `take` (accept upstream as merged), `override` (keep upstream code but change behavior on the fork), `revert` (undo the upstream change on the fork), `flag-off` (keep the code, disabled by default). |
| Fork commit | Short SHA (or `fork#NN` PR) that implements it. `none` for a `take`. |
| Reason | One line, why. |
| Date | `YYYY-MM-DD` the decision was made. |
| Re-check | What would make us revisit it (`when upstream touches X`, `never`). |

Rules for agents:

- A sync PR triage pre-fills any item whose upstream ref or area matches a row here.
  Keep `Upstream ref` and `Area` specific enough that the match is unambiguous.
- Prefer `override`/`flag-off` over `revert` when the fork only needs different
  behavior: reverts conflict again on every later sync, overrides usually do not.
- If an upstream change touches files listed in a row's `Re-check`, that sync PR
  must call it out in the triage even when it is not otherwise risky.
- Never delete a row; mark a stale one `superseded by <ref>` in its Re-check cell.

## Standing fork deviations

| Upstream ref | Area | Decision | Fork commit | Reason | Date | Re-check |
|---|---|---|---|---|---|---|
| - | telemetry | flag-off | 0b31831c1 (fork#59) | Telemetry, analytics and error reporting are off by default; the fork does not send data to upstream vendors. | 2026-10-12 | Any upstream change adding a network call, analytics event, PostHog/Sentry hook or telemetry default. |
| - | ci / release | override | 095153ad8, 884303aa6 | Fork-only workflows: `fork-release.yml` (FORK_VERSION / `v*` tag releases of mac arm64 + linux AppImage), `build-artifacts.yml`, `sync-upstream*.yml`, Pullfrog. | 2026-10-12 | Upstream edits to `.github/workflows/`, `FORK_VERSION`, or release scripts under `frontend/scripts/`. |
| - | ci / release | override | 095153ad8 (fork#56) | macOS signing and notarization use the fork's own Apple secrets; unsigned fallback when they are absent. | 2026-10-12 | Upstream changes to electron-builder / osx-sign config, entitlements, or `docs/macos-signing.md`. |
| - | ci | flag-off | fork#16 | Upstream-only workflows are skipped on the fork with a job-level `if: github.repository == 'OrchestratorInc/agent-orchestrator'` instead of being deleted, to keep merges clean. Listed in `docs/fork-ci.md`. | 2026-10-08 | A new upstream workflow: gate it the same way or take it deliberately. |
| - | updater | override | fork-release.yml (build check) | The updater feed repo is baked to `github.repository` (this fork) at build time and verified by the build; releases publish `latest-*.yml`. | 2026-10-08 | Upstream changes to the updater provider, feed script (`frontend/scripts/feed.mjs`), app name or bundle id. |
| #55 | artifacts | override | 63575d4b5 (fork#55) | Artifact sessions do not start the default pipeline. | 2026-10-12 | Upstream changes to artifact spawn, pipeline defaults or `artifact view` requests. |
| - | ci | override | fork#CI-usage | CI minutes are capped on the Free plan (macOS bills 10x): PR CI skips drafts and runs on `ready_for_review`; the macOS jobs (frontend `mac-update-helper`, CLI E2E macOS leg) run on `main` pushes only; `frontend.yml` no longer triggers on any `.github/workflows/**` change. | 2026-10-14 | When the plan or the included minutes change, or upstream rewrites go.yml / frontend.yml / cli-e2e.yml. |
| #54 | billing | take | none | Billing P1s from review judged out of scope: the fork does not use AO Cloud billing. Not fixed or reverted. | 2026-10-12 | Only if the fork starts using AO Cloud billing. |

## Sync decisions

Decisions from upstream sync triage. Add one row per rejected/overridden item (items
that are simply `take` need no row unless noteworthy).

| Upstream ref | Area | Decision | Fork commit | Reason | Date | Re-check |
|---|---|---|---|---|---|---|
| _pending_ | _pending_ | _pending_ | _pending_ | Decisions from the #69 triage (upstream `032ed6f`, see `docs/upstream-sync/2026-10-14-032ed6f.md` once it lands). | _pending_ | - |
