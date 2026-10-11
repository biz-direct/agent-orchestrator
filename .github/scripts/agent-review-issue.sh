#!/usr/bin/env bash
# Open, comment on, or close an `agent-review` issue (a human is needed).
#
#   agent-review-issue.sh open  "<title>" "<body>"
#   agent-review-issue.sh close "<title>" "<comment>"
#
# Issues are matched by exact title so a retry never opens a duplicate, even if
# the label failed to attach. Uses the REST API: GraphQL createIssue is refused
# for SYNC_PAT. Needs GH_TOKEN and GITHUB_REPOSITORY.
set -euo pipefail

action="${1:?open|close}"
title="${2:?title}"
body="${3:?body}"
repo="${GITHUB_REPOSITORY:?}"
label="agent-review"

existing="$(TITLE="$title" gh api "repos/$repo/issues?state=open&per_page=100" --paginate \
  --jq '.[] | select(.pull_request | not) | select(.title == env.TITLE) | .number' | head -n1)"

case "$action" in
  open)
    gh api "repos/$repo/labels" -f name="$label" -f color=D93F0B \
      -f description="Automated workflow needs a human decision" >/dev/null 2>&1 || true
    if [ -n "$existing" ]; then
      gh api "repos/$repo/issues/$existing/comments" -f body="$body" >/dev/null
      echo "Commented on #$existing"
    else
      number="$(gh api "repos/$repo/issues" -f title="$title" -f body="$body" \
        -f "labels[]=$label" --jq .number)"
      echo "Opened issue #$number"
    fi
    ;;
  close)
    if [ -n "$existing" ]; then
      gh api "repos/$repo/issues/$existing/comments" -f body="$body" >/dev/null
      gh api -X PATCH "repos/$repo/issues/$existing" -f state=closed >/dev/null
      echo "Closed #$existing"
    fi
    ;;
  *)
    echo "unknown action: $action" >&2
    exit 2
    ;;
esac
