#!/usr/bin/env bash
# Regression tests for sync-upstream-gate.sh, driven by a fake `gh`.
#
#   bash .github/scripts/sync-upstream-gate.test.sh
#
# Each gate invocation below stands for one trigger (a CI workflow finishing, a
# Pullfrog review, a Pullfrog run finishing). The fake gh returns the PR state
# as it would look at that moment; the test asserts the merge decision.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
gate="$here/sync-upstream-gate.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"

# Fake gh. State comes from files in $FAKE_DIR:
#   pr.json       `gh pr view` output
#   reviews.json  reviews array
#   checks.N      `gh pr checks` output for the Nth call (last one repeats)
#   calls.log     every call, for assertions
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $*" >> "$FAKE_DIR/calls.log"
case "$1 $2" in
  "pr view") cat "$FAKE_DIR/pr.json" ;;
  "pr checks")
    n=$(($(cat "$FAKE_DIR/checks.count" 2>/dev/null || echo 0) + 1))
    echo "$n" > "$FAKE_DIR/checks.count"
    f="$FAKE_DIR/checks.$n"
    [ -f "$f" ] || f="$(ls "$FAKE_DIR"/checks.[0-9]* | grep -v count | sort -V | tail -n1)"
    cat "$f"
    ;;
  "pr merge") ;;
  api*)
    case "$2" in
      */reviews) cat "$FAKE_DIR/reviews.json" ;;
      *) echo "[]" ;;
    esac
    ;;
esac
EOF
chmod +x "$work/bin/gh"

fails=0
SHA=abc123

reset() {
  export FAKE_DIR="$work/case"
  rm -rf "$FAKE_DIR"
  mkdir -p "$FAKE_DIR"
  : >"$FAKE_DIR/calls.log"
  cat >"$FAKE_DIR/pr.json" <<JSON
{"url":"https://x/pr/7","state":"OPEN","baseRefName":"main","headRefName":"sync/upstream-aaa","headRefOid":"$SHA","headRepository":{"name":"r"},"headRepositoryOwner":{"login":"o"}}
JSON
  echo "[]" >"$FAKE_DIR/reviews.json"
}

review() { # <commit> <first line of body>
  jq -n --arg c "$1" --arg b "$2" \
    '[{user:{login:"pullfrog[bot]"},commit_id:$c,html_url:"https://x/r",body:($b+"\n\nReviewed")}]' \
    >"$FAKE_DIR/reviews.json"
}

checks() { # <n> <json>
  echo "$2" >"$FAKE_DIR/checks.$1"
}

gate() { # runs the gate as one trigger; output in $out
  out="$(PATH="$work/bin:$PATH" GITHUB_REPOSITORY=o/r bash "$gate" 7 2>&1)" || true
}

merged() { grep -q '^gh pr merge 7 --merge' "$FAKE_DIR/calls.log"; }
issue_opened() { grep -q 'repos/o/r/issues' "$FAKE_DIR/calls.log" && grep -q -- '-f title=' "$FAKE_DIR/calls.log"; }

expect() { # <name> <condition: merge|nomerge|issue>
  local ok=0
  case "$2" in
    merge) merged && ok=1 ;;
    nomerge) merged || ok=1 ;;
    issue) issue_opened && ! merged && ok=1 ;;
  esac
  if [ "$ok" = 1 ]; then
    echo "ok   - $1"
  else
    echo "FAIL - $1 (expected $2)"
    echo "       ${out//$'\n'/$'\n       '}"
    fails=$((fails + 1))
  fi
}

PASS='{"name":"scan","bucket":"pass"},{"name":"test","bucket":"pass"}'
CLEAN="> ✅ No new issues found."

# --- The reported scenario: CI done -> clean review while Pullfrog still pending
# -> Pullfrog done. Each step is a separate trigger; no push, CI rerun or
# schedule in between.
reset
checks 1 "[$PASS,{\"name\":\"Pullfrog\",\"bucket\":\"pending\"}]"
gate; expect "CI complete, no review yet, Pullfrog pending: waits" nomerge

review "$SHA" "$CLEAN"
gate; expect "clean review, Pullfrog check still pending: waits" nomerge

checks 1 "[$PASS,{\"name\":\"Pullfrog\",\"bucket\":\"pass\"},{\"name\":\"sync-gate\",\"bucket\":\"pending\"}]"
rm -f "$FAKE_DIR/checks.count"
gate; expect "Pullfrog complete (gate's own check pending is ignored): merges" merge

# --- The Pullfrog check lags its workflow run: poll until it settles.
reset
review "$SHA" "$CLEAN"
checks 1 "[$PASS,{\"name\":\"Pullfrog\",\"bucket\":\"pending\"}]"
checks 2 "[$PASS,{\"name\":\"Pullfrog\",\"bucket\":\"pending\"}]"
checks 3 "[$PASS,{\"name\":\"Pullfrog\",\"bucket\":\"pass\"}]"
out="$(PATH="$work/bin:$PATH" GITHUB_REPOSITORY=o/r WAIT_SECS=20 POLL_SECS=1 bash "$gate" 7 2>&1)" || true
expect "WAIT_SECS polls pending checks until Pullfrog settles: merges" merge

reset
review "$SHA" "$CLEAN"
checks 1 "[$PASS,{\"name\":\"Pullfrog\",\"bucket\":\"pending\"}]"
out="$(PATH="$work/bin:$PATH" GITHUB_REPOSITORY=o/r WAIT_SECS=2 POLL_SECS=1 bash "$gate" 7 2>&1)" || true
expect "polling gives up when the check never settles: no merge" nomerge

# --- Existing safeguards.
reset
review "$SHA" "$CLEAN"
checks 1 "[$PASS,{\"name\":\"CodeRabbit\",\"bucket\":\"pending\"}]"
gate; expect "CodeRabbit pending is ignored: merges" merge

reset
review "$SHA" "$CLEAN"
checks 1 '[{"name":"scan","bucket":"pass"},{"name":"test","bucket":"fail"}]'
gate; expect "failed CI: opens an issue, no merge" issue

reset
review "$SHA" "> [!IMPORTANT]"
checks 1 "[$PASS]"
gate; expect "Pullfrog findings: opens an issue, no merge" issue

reset
review "old" "$CLEAN"
checks 1 "[$PASS]"
gate; expect "review of an older commit does not count: no merge" nomerge

reset
review "$SHA" "$CLEAN"
checks 1 '[{"name":"test","bucket":"pass"}]'
gate; expect "scan never reported: no merge" nomerge

reset
review "$SHA" "$CLEAN"
: >"$FAKE_DIR/checks.1"
gate; expect "no checks reported yet: no merge" nomerge

reset
jq '.headRefName="feature/x"' "$FAKE_DIR/pr.json" >"$FAKE_DIR/pr.tmp" && mv "$FAKE_DIR/pr.tmp" "$FAKE_DIR/pr.json"
review "$SHA" "$CLEAN"
checks 1 "[$PASS]"
gate; expect "non-sync branch is never merged" nomerge

reset
jq '.headRepositoryOwner.login="evil"' "$FAKE_DIR/pr.json" >"$FAKE_DIR/pr.tmp" && mv "$FAKE_DIR/pr.tmp" "$FAKE_DIR/pr.json"
review "$SHA" "$CLEAN"
checks 1 "[$PASS]"
gate; expect "head from an outside fork is never merged" nomerge

if [ "$fails" -ne 0 ]; then
  echo "$fails test(s) failed"
  exit 1
fi
echo "all gate tests passed"
