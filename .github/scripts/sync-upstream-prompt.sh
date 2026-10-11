#!/usr/bin/env bash
# Print the Pullfrog prompt that resolves the conflicts of a draft sync PR.
#
#   sync-upstream-prompt.sh <pr-number> <upstream-sha>   (UPSTREAM env: owner/repo)
set -euo pipefail

pr="${1:?pr number}"
sha="${2:?upstream sha}"
upstream="${UPSTREAM:?}"

cat <<PROMPT
Resolve the merge conflicts in draft pull request #$pr (branch sync/upstream-$sha). Upstream is https://github.com/$upstream.git (main, commit $sha).

The branch already holds a commit that merged upstream into this fork's main with the conflict markers still in the files. Do not redo the merge, create another branch or open another pull request.

1. Check out the branch. The "Conflict inventory" section of the pull request description lists every path that conflicted, with its git status code. Some conflicts leave no markers in the committed files (modify/delete, binary, renames, file versus directory), so work from that list, not only from a marker scan; also run: git grep -lIE '^(<<<<<<<|>>>>>>>) ' to find files that still contain markers. If the description has no inventory (an older run), compare against a fresh merge: git merge-tree --write-tree --name-only origin/main FETCH_HEAD after fetching upstream's main.
2. Resolve every conflict. Keep this fork's own changes (fork CI workflows under .github/workflows, fork release and versioning) and keep upstream's intent for everything else. For generated files (sqlc output under backend/internal/storage/sqlite/gen, OpenAPI and API schema files) take upstream's version, or regenerate only if that takes one quick command. Merge the i18n JSON files by keeping the union of keys.
3. You have a hard one-hour limit and unpushed work is lost. Commit and push to this same branch after every batch of about ten files (plain git push, never force). If you run low on time, push what you have; a later run continues from the branch.
4. Do NOT run test suites, linters or long validation; CI on the pull request does that. When no markers remain, run "go build ./..." in backend and, if it is quick, the frontend typecheck, once. Note anything you skipped.
5. Then update the pull request description to list each conflicted file and how you resolved it, and anything you are unsure about, keeping the inventory, and mark the pull request ready for review (gh pr ready). Only do that once every path in the inventory is resolved. Do not merge it.

If a conflict cannot be resolved with confidence, resolve it as well as you can and say exactly what needs a human.
PROMPT
