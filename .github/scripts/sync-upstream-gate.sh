#!/usr/bin/env bash
# Decide what happens to an upstream-sync PR:
#   - Pullfrog reviewed the current head clean AND CI is green  -> merge it
#   - a check failed, or Pullfrog reported findings             -> open an
#                                                                  agent-review issue
#   - anything still running / not yet reviewed                 -> wait
#   - a draft PR (Pullfrog still resolving conflicts)           -> wait; once no
#                                                                  conflict marker
#                                                                  is left, mark
#                                                                  it ready
#
#   sync-upstream-gate.sh <pr-number>
#
# Pullfrog posts COMMENTED reviews as pullfrog[bot]. A review whose first
# non-blank line starts with "> ✅" means no issues; any other body (for example a
# "> [!IMPORTANT]" block) means findings. Only a review of the PR's current head
# commit counts, so a push invalidates the verdict until Pullfrog re-reviews.
#
# Env: GH_TOKEN, GITHUB_REPOSITORY; optional PULLFROG_LOGIN (default
# pullfrog[bot]), IGNORED_CHECKS (default CodeRabbit,sync-gate,sync-gate-failure),
# WAIT_SECS/POLL_SECS to poll pending checks, DRY_RUN=1 to print actions instead
# of taking them (and to skip the PR guards, for testing).
set -euo pipefail

pr="${1:?pr number}"
repo="${GITHUB_REPOSITORY:?}"
reviewer="${PULLFROG_LOGIN:-pullfrog[bot]}"
ignored="${IGNORED_CHECKS:-CodeRabbit,sync-gate,sync-gate-failure}"
dry="${DRY_RUN:-}"
here="$(cd "$(dirname "$0")" && pwd)"

act() {
  if [ -n "$dry" ]; then echo "DRY_RUN: $*"; else "$@"; fi
}

pr_json="$(gh pr view "$pr" --json url,state,isDraft,baseRefName,headRefName,headRefOid,headRepository,headRepositoryOwner)"
url="$(jq -r .url <<<"$pr_json")"
state="$(jq -r .state <<<"$pr_json")"
base="$(jq -r .baseRefName <<<"$pr_json")"
head_ref="$(jq -r .headRefName <<<"$pr_json")"
head_sha="$(jq -r .headRefOid <<<"$pr_json")"
is_draft="$(jq -r '.isDraft // false' <<<"$pr_json")"
head_repo="$(jq -r '.headRepositoryOwner.login + "/" + .headRepository.name' <<<"$pr_json")"

guard_failed=""
[ "$state" = OPEN ] || guard_failed="PR #$pr is $state"
[ "$base" = main ] || guard_failed="PR #$pr targets $base, not main"
[[ "$head_ref" == sync/upstream-* ]] || guard_failed="PR #$pr head $head_ref is not a sync/upstream-* branch"
[ "$head_repo" = "$repo" ] || guard_failed="PR #$pr head repo is $head_repo, not $repo"
if [ -n "$guard_failed" ]; then
  echo "$guard_failed; nothing to do."
  [ -n "$dry" ] || exit 0
  echo "(DRY_RUN: continuing anyway)"
fi

# Conflict markers: more marker lines in the PR head's tree than on main means
# the merge is not resolved yet (a diff is no use: GitHub refuses diffs this large).
# Checked on every run: a draft is held until they are gone, and nothing with a
# marker is ever merged. Needs a checkout of main to compare against.
count_markers() {
  { git grep -hIE '^(<<<<<<<|>>>>>>>) ' "$1" -- 2>/dev/null || true; } | wc -l | tr -d ' '
}
if ! git fetch -q --depth=1 "https://x-access-token:${GH_TOKEN:-}@github.com/$repo.git" "pull/$pr/head" 2>/dev/null; then
  echo "::warning::Could not fetch the head of PR #$pr to look for conflict markers; leaving it."
  exit 0
fi
markers=false
if [ "$(count_markers FETCH_HEAD)" -gt "$(count_markers origin/main)" ]; then markers=true; fi

if [ "$is_draft" = true ]; then
  if [ "$markers" = true ]; then
    echo "PR #$pr is a draft with unresolved conflict markers; Pullfrog is still resolving."
  else
    echo "PR #$pr is a draft with no conflict markers left; marking it ready for review."
    act gh pr ready "$pr"
  fi
  exit 0
fi

# CI: gh pr checks exits non-zero while checks are pending or failed but still
# prints the JSON; "no checks reported" prints nothing. The gate's own jobs and
# CodeRabbit are ignored so the gate never waits on itself. With WAIT_SECS set,
# pending checks are polled for that long: Pullfrog's check can lag its
# workflow run finishing, and nothing re-triggers the gate after that.
deadline=$((SECONDS + ${WAIT_SECS:-0}))
while :; do
  checks="$(gh pr checks "$pr" --json name,bucket 2>/dev/null || true)"
  [ -n "$checks" ] || checks="[]"
  checks="$(jq -c --arg ignored "$ignored" \
    '($ignored | split(",")) as $skip | map(select(.name as $n | $skip | index($n) | not))' <<<"$checks")"
  pending="$(jq -r '[.[] | select(.bucket == "pending") | .name] | join(", ")' <<<"$checks")"
  if [ -n "$pending" ] && [ "$SECONDS" -lt "$deadline" ]; then
    echo "Waiting for checks: $pending"
    sleep "${POLL_SECS:-15}"
    continue
  fi
  break
done
failed="$(jq -r '[.[] | select(.bucket == "fail" or .bucket == "cancel") | .name] | join(", ")' <<<"$checks")"
scan_ok="$(jq -r 'any(.[]; .name == "scan" and .bucket == "pass")' <<<"$checks")"

# Pullfrog verdict on the current head commit.
review="$(gh api "repos/$repo/pulls/$pr/reviews" --paginate |
  jq -s --arg who "$reviewer" --arg sha "$head_sha" \
    'add // [] | map(select(.user.login == $who and .commit_id == $sha)) | last // empty')"
verdict=waiting
review_url=""
if [ -n "$review" ]; then
  review_url="$(jq -r .html_url <<<"$review")"
  first_line="$(jq -r .body <<<"$review" | grep -m1 '[^[:space:]]' || true)"
  if [[ "$first_line" == '> ✅'* ]]; then verdict=clean; else verdict=findings; fi
fi

echo "PR #$pr @ ${head_sha:0:12}: verdict=$verdict failed=[${failed}] pending=[${pending}] scan_ok=$scan_ok markers=$markers"

title="Upstream sync needs review: PR #$pr"

if [ -n "$failed" ] || [ "$verdict" = findings ] || [ "$markers" = true ]; then
  reasons=""
  [ -z "$failed" ] || reasons="$reasons- Failing checks: $failed"$'\n'
  [ "$markers" != true ] || reasons="$reasons- Unresolved conflict markers remain in the diff"$'\n'
  [ "$verdict" != findings ] || reasons="$reasons- Pullfrog reported findings: $review_url"$'\n'
  body="The automatic merge of $url was stopped because a human is needed.

$reasons
Fix it on the PR branch (Pullfrog re-reviews new commits and this gate re-runs on each review and CI result), or merge/close the PR yourself. This issue closes itself if the PR later merges through the gate."
  act bash "$here/agent-review-issue.sh" open "$title" "$body"
  act gh pr merge "$pr" --disable-auto >/dev/null 2>&1 || true
  exit 0
fi

if [ "$verdict" = clean ] && [ -z "$pending" ] && [ "$scan_ok" = true ]; then
  echo "Pullfrog is clean and CI is green; merging #$pr."
  act gh pr merge "$pr" --merge --match-head-commit "$head_sha"
  act bash "$here/agent-review-issue.sh" close "$title" "Merged automatically after a clean Pullfrog review and green CI: $url"
  exit 0
fi

echo "Waiting: verdict=$verdict pending=[${pending}] scan_ok=$scan_ok"
