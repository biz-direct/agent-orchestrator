# Fork CI (biz-direct/agent-orchestrator)

## Builds and releases
- `build-artifacts.yml` builds unsigned **macOS Apple Silicon** (zip) and **Linux x64** (AppImage) only. It is `workflow_dispatch` (inputs `ref`, `version`) and `workflow_call`.
- Pushing a `v*` tag runs `fork-release.yml`: builds both, then creates a GitHub Release with the artifacts, `digests.json` and `SHA256SUMS.txt`. Cut a release with `git tag v0.1.0 && git push origin v0.1.0`.
- Required repo **variable** `VITE_WORKOS_CLIENT_ID` (Settings → Secrets and variables → Actions → Variables); the build fails with a clear error if it is absent.
- The baked updater feed repo is `github.repository`; the build verifies it. No `latest-mac.yml` is published, so in-app auto-update does not work; install new versions manually.

## Installing
- **macOS:** unzip, move to Applications, then right-click → Open (first launch), or run `xattr -cr "/Applications/Agent Orchestrator.app"`.
- **Arch/Omarchy:** `chmod +x agent-orchestrator-linux-x64.AppImage && ./agent-orchestrator-linux-x64.AppImage` (needs `fuse2`; or run with `--appimage-extract-and-run`).

## Upstream sync
`sync-upstream.yml` runs daily (and on dispatch): merges `OrchestratorInc/agent-orchestrator` main into a `sync/upstream-<sha>` branch, opens a PR into `main` and merges it (auto-merge when checks are required, direct otherwise). On conflicts it opens an `upstream-sync-conflict` issue and pushes nothing. Needs secret `SYNC_PAT` (repo + workflow scopes) and "Allow auto-merge" enabled in repo settings.

## Disabled on the fork
`release-latest-guard`, `pr-review-leaderboard`, `deploy-docs` and `mac-update-e2e` are gated with `if: github.repository == '<upstream>'`.
