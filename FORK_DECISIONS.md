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
| #6575 (narrowed), #7119, #6999 | chat / background tasks | take (retest) | fork#75 | The #6575 revert stays for the Tab-autocomplete UI, endpoint and runner, but later upstream fixes (#7119 title/branch generation, #6999 Interrupted cards) build on its non-autocomplete parts: read-only/no-network background tasks (`DisableNetwork`, `DisableTools`), the Codex driver changes, `LatestConversationTurn`. Those are kept. They send nothing off-device and add no model spend. | 2026-10-25 | If upstream moves background tasks off these fields. Autocomplete itself: restore by reverting 1263182a5. |
| #6630 / #7087 | sessions / artifacts | revert (kept #7087) | fork#75 | `deleteArtifacts` on archive stays reverted (data-loss defects, see the #6630 row). Upstream #7087 (delete a file's version history when the user deletes that file) is kept: `DeleteVersions` only, no whole-session delete, no trash directory. | 2026-10-25 | When upstream stabilizes archive-delete. Restore: re-add `DeleteSession` from upstream `delete.go`. |
| - | db / migrations | override | fork#75 | Upstream `0197_detach_split_orchestrator_conversations` collides with the fork's shipped `0197_session_requested_output`, and upstream `0200_session_artifact_version_source` must run after the fork's `0252_session_artifact_versions`. They are carried as `0253_detach_split_orchestrator_conversations` and `0254_session_artifact_version_source`; all other upstream migrations keep their numbers. Fork DBs never recorded upstream 0197/0200, so no ledger repair is needed. | 2026-10-25 | Every sync: new upstream numbers 0198-0252 may collide with fork 0250-0252. Next free fork number: 0255. |
| #6425, #6979, #7112 | telemetry | flag-off | fork#75 | Upstream's in-app opt-out marker (`telemetry_opt_out`, `applyAnalyticsOptOut`, identify-on-sign-in, updater-failure dedup) is kept but layered under the fork's single opt-in: opting out tears the PostHog client down through the consent path (no `reset()`), re-arming never turns telemetry on by itself, `packagedDefault` stays ignored, policy records older than schema v3 read as off, and the daemon's `EventsAllowed` still needs the explicit env plus a v3 policy. Upstream's docs/FAQ text keeps the fork's off-by-default wording. | 2026-10-25 | Any upstream change to `telemetry.ts`, `telemetry-policy*.ts`, `policyauthority` or `config` defaults. |
| #6588, #7124, #7126, #6743 | chat / agent switching | take | fork#75 | Upstream replaced agent switching with provider switching and removed `agent_switching*.go`, the agent-switch observability/visibility code and the daemon telemetry policy client. Taken as shipped. Fork follow-ups: `ErrSwitchInProgress` is now `ErrOperationInProgress` in crew/pipeline code; the Prompts freshness test that depended on the old switch path was dropped (a provider switch reuses the session's stored prompt, so rule freshness is unchanged by it). | 2026-10-25 | If provider switching starts rebuilding the system prompt: call `commitAppliedRules` there. |
| #6995 | ci / contributor project | flag-off | fork#75 | `contributor-project.yml` was already gated to the upstream repo. `harness-release-feeds.yml` (live vendor-feed check, runs on PRs touching `systeminstall`) is now gated the same way so it does not spend fork CI minutes or fail on third-party outages. Fork workflow trims (draft skip, `ready_for_review`, macOS on main only, `build-artifacts` signing) are kept; upstream's clang++-15 step is added to `build-artifacts.yml`. | 2026-10-25 | When upstream changes these workflows. |
| - | pipelines / crews | take (retest) | fork#75 | Conflicts in `daemon`, `lifecycle_wiring`, `review`, `session_manager`, `SessionView`, `TaskComposer` and `openapi` resolved to keep both sides: pipeline run/gates/guard, crew roles and baton, Prompts and global worker rules, and upstream's new features (provider switching, artifact version history, version refresh, harness defaults, task naming). i18n keys are the union of both. | 2026-10-25 | Next sync: re-run pipeline, crew and Prompts tests. |
| #6575 | chat | revert | 1263182a5 | Tab autocomplete makes a hidden model call on the user's provider after every settled turn, sending the transcript, with no off switch. Depends on #6616 (kept). | 2026-10-14 | When upstream adds an off-by-default setting for it. Restore: revert 1263182a5. |
| #6641 | inspector / spend | revert | 543716e41 | Upstream hides session cost and token usage; the fork keeps the in-app spend display. | 2026-10-14 | When upstream restores a spend display or touches the Usage & cost section. Restore: revert 543716e41. |
| #6647 | desktop / naming | revert | 8e366119e | Upstream sets the display name to "Orchestrator.inc"; the fork keeps "Agent Orchestrator". | 2026-10-14 | Any upstream change to `APP_DISPLAY_NAME`, app name, bundle id or feed names. Restore: revert 8e366119e. |
| #6654 | desktop (Windows) | revert | 375317e2c | Upstream sets the NSIS `productName` to "Orchestrator.inc"; the fork keeps "Agent Orchestrator". | 2026-10-14 | Same as #6647. Restore: revert 375317e2c. |
| #6583 | artifacts / db | override | 16f8b6ab0 | Upstream migration `0196_session_artifact_versions` collides with the fork's `0196_conversation_message_sender`. Renumbered to `0252` (above the fork's highest, `0251_crew_baton`); `repairForkMigrationCollisions` records 0252 when the table exists and drops an ambiguous 0196 entry when the sender columns are absent. | 2026-10-14 | Any new upstream migration at or above 0196, and each sync (renumber again, never reuse 0198). Restore: not applicable (renumber is required while the fork owns 0196). |
| #6630 | sessions / artifacts | revert | dcd226875 | Upstream "keep or delete artifacts when archiving" (`e2cb54343`) added a destructive `deleteArtifacts` option to the kill endpoint. Pullfrog found seven consecutive data-loss/race defects in it (restore racing the delete, the archive generation, background cleanup, partial deletion and rollback, and a sweepable recovery copy); each fix exposed a deeper edge. Archive now always keeps artifacts. | 2026-10-24 | When upstream stabilizes it (look for a single guard shared with restore, and a delete that commits the DB rows before any irreversible file removal). Restore: revert dcd226875, then re-run `npm run api` and `npm run sqlc`. |
| #6571, #6586 | chat / file links | take (retest) | 16f8b6ab0 | Artifact links and absolute-path file links merged with fork #26 clickable paths: the catalog decides what is a file link, absolute references open as written so the daemon resolves them. | 2026-10-14 | Upstream edits to `ChatMarkdown.tsx`, `workspace-file-path.ts`, or the file-link route. |
| #6611 | desktop / telemetry | take (retest) | 16f8b6ab0 | Lazy PostHog SDK import merged with fork #59 opt-in: the consent epoch is re-checked after the async import; no telemetry default or event changed. | 2026-10-14 | Any upstream change to `telemetry.ts` or telemetry defaults. |
| #6582 | prompts | take (retest) | 16f8b6ab0 | External PR review routing merged with fork Prompts (#62/#65/#67) and pipeline guidance; both prompt tests kept. | 2026-10-14 | Upstream edits to orchestrator/worker system prompts. |
| #6579 | mobile | take | none | Mobile app rename taken as upstream ships it. | 2026-10-14 | Revisit if the fork ships its own mobile app. |
