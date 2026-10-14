#!/usr/bin/env bash
# Print the Pullfrog prompt for the one triage pass on a draft sync PR: resolve any
# merge conflicts, then write the triage list into the PR body. One pass per sync.
#
#   sync-upstream-prompt.sh <pr-number> <upstream-sha>   (UPSTREAM env: owner/repo)
set -euo pipefail

pr="${1:?pr number}"
sha="${2:?upstream sha}"
upstream="${UPSTREAM:?}"

cat <<PROMPT
Triage draft pull request #$pr (branch sync/upstream-$sha). Upstream is https://github.com/$upstream.git (main, commit $sha). The branch merges upstream into this fork's main. You do this in ONE pass; do not start sub-tasks, extra reviews or a second pass.

The pull request must stay a DRAFT. Never mark it ready, never merge it, never approve it, never open another branch or pull request.

Part A - only if the description has a "Conflict inventory" section
The branch holds a commit with the conflict markers still in the files. Do not redo the merge.
1. The inventory lists every conflicted path with its git status code. Some conflicts leave no markers (modify/delete, binary, renames, file versus directory), so work from that list; also run: git grep -lIE '^(<<<<<<<|>>>>>>>) ' to find files that still contain markers.
2. Resolve every conflict. Keep this fork's own changes (fork CI workflows under .github/workflows, fork release and versioning, everything recorded in FORK_DECISIONS.md) and keep upstream's intent for everything else. For generated files (sqlc output under backend/internal/storage/sqlite/gen, OpenAPI and API schema files) take upstream's version, or regenerate only if that takes one quick command. Merge the i18n JSON files by keeping the union of keys.
3. You have a hard one-hour limit and unpushed work is lost. Commit and push to this same branch after every batch of about ten files (plain git push, never force).
4. Do NOT run test suites, linters or long validation; CI on the pull request does that. When no markers remain, run "go build ./..." in backend and, if it is quick, the frontend typecheck, once. Note anything you skipped.
5. Keep the inventory in the description and add a short list of how each conflicted file was resolved, plus anything you are unsure about.

Part B - the triage list (always)
1. Fetch enough history: git fetch --no-tags origin main sync/upstream-$sha, deepening or unshallowing if needed. The upstream changes in this sync are: git log --first-parent --format='%h %s' origin/main..\$(git rev-parse $sha) (the pinned upstream commit $sha is in the branch history even after resolution commits were pushed, so never derive it from the branch tip's parents; use --no-merges if first-parent is too coarse). One upstream PR is usually one commit, with its number in the subject as (#NNNN).
2. Read FORK_DECISIONS.md. It records decisions already made about upstream changes (columns: upstream ref, area, decision, fork commit, reason, date, re-check).
3. Compute the paths the fork changed since it diverged: git diff --name-only \$(git merge-base origin/main \$(git rev-parse $sha)) origin/main. An upstream commit that touches any of those paths touches a file covered by a fork override.
4. Replace the line "<!-- sync-triage:pending -->" under the "## Triage" heading of the description with the triage list, using gh pr edit. One checkbox line per upstream PR or commit, newest first, in exactly this form:
   - [ ] #NNNN or <short sha> | area | one-line summary | risk: low/med/high | recommend: take/override/revert
   Leave every box unticked. The user ticks a box to REJECT an item; a ticked box means a worker adds a revert or override for it.
5. Items already decided in FORK_DECISIONS.md (matching upstream ref or area): pre-fill the recommendation with that decision and append "| decided: FORK_DECISIONS.md (<decision>)".
6. Flag explicitly, with a "FLAG:" tag on the line plus the reason, any item that: changes the app name, bundle id, updater (feed, provider, channel), or code signing / notarization; adds or changes telemetry, analytics, error reporting, or any outbound network call; or touches a path found in step 3. Flagged items get risk med at least. Judge by the diff, not the title.
7. Keep each line to one line. Do not paste diffs. Group nothing; do not add commentary beyond a two-line overview above the list (counts of items and flags).
8. Last, add the line "<!-- sync-triage:done -->" right under the "## Triage" heading. It tells the workflow the pass finished, so a later run does not repeat it (and the cost). Add it only once Part A (if any) and Part B are fully done.

Do not apply rejections yourself; a person ticks the boxes and a worker applies them.
If a conflict cannot be resolved with confidence, resolve it as well as you can and say exactly what needs a human in the description.
PROMPT
