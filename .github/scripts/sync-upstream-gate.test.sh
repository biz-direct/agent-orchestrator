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
  "pr ready") ;;
  api*)
    case "$2" in
      */reviews) cat "$FAKE_DIR/reviews.json" ;;
      *) echo "[]" ;;
    esac
    ;;
esac
EOF
chmod +x "$work/bin/gh"

# Fake git: fetching succeeds; `git grep -c` prints "<rev>:path:count" for the
# marker files listed in main_markers / head_markers ("path:count" per line).
cat >"$work/bin/git" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  fetch) exit 0 ;;
  grep)
    rev=origin/main; f="$FAKE_DIR/main_markers"
    for a in "$@"; do [ "$a" = FETCH_HEAD ] && rev=FETCH_HEAD && f="$FAKE_DIR/head_markers"; done
    [ -s "$f" ] && sed "s#^#$rev:#" "$f"
    exit 0
    ;;
esac
exit 1
EOF
chmod +x "$work/bin/git"

fails=0
SHA=abc123

reset() {
  export FAKE_DIR="$work/case"
  rm -rf "$FAKE_DIR"
  mkdir -p "$FAKE_DIR"
  : >"$FAKE_DIR/calls.log"
  cat >"$FAKE_DIR/pr.json" <<JSON
{"url":"https://x/pr/7","state":"OPEN","isDraft":false,"baseRefName":"main","headRefName":"sync/upstream-aaa","headRefOid":"$SHA","headRepository":{"name":"r"},"headRepositoryOwner":{"login":"o"}}
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
    hold) ! merged && ! issue_opened && ok=1 ;;
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

# --- Draft sync PRs: Pullfrog resolves the conflicts on the branch and marks
# the PR ready itself; the gate never promotes one.
draft() { jq '.isDraft=true' "$FAKE_DIR/pr.json" >"$FAKE_DIR/pr.tmp" && mv "$FAKE_DIR/pr.tmp" "$FAKE_DIR/pr.json"; }
head_markers() { printf '%s\n' "$@" >"$FAKE_DIR/head_markers"; }
main_markers() { printf '%s\n' "$@" >"$FAKE_DIR/main_markers"; }

reset
draft; head_markers "a.go:2"
review "$SHA" "$CLEAN"
checks 1 "[$PASS]"
gate; expect "draft with conflict markers: held, no merge, no issue" hold

reset
draft
review "$SHA" "$CLEAN"
checks 1 "[$PASS]"
gate; expect "draft without markers is not promoted or merged by the gate" hold

reset
head_markers "a.go:2"
review "$SHA" "$CLEAN"
checks 1 "[$PASS]"
gate; expect "ready PR that still has conflict markers: issue, no merge" issue

reset
main_markers "docs/x.md:2"; head_markers "docs/x.md:2"
review "$SHA" "$CLEAN"
checks 1 "[$PASS]"
gate; expect "markers already on main (docs, fixtures) do not block: merges" merge

reset
main_markers "docs/x.md:2"; head_markers "a.go:2"
review "$SHA" "$CLEAN"
checks 1 "[$PASS]"
gate; expect "equal marker totals: removed fixture markers do not hide a new conflict" issue

reset
main_markers "docs/x.md:2"; head_markers "docs/x.md:4"
review "$SHA" "$CLEAN"
checks 1 "[$PASS]"
gate; expect "more markers in a file that already had some: issue" issue

# --- Event wiring: the gate only re-runs when a workflow listed under
# workflow_run completes, so every pull_request workflow must be listed.
wiring="$(python3 -I - "$here/../workflows" <<'PY'
import glob, sys, yaml
dirpath = sys.argv[1]
gate = yaml.safe_load(open(f"{dirpath}/sync-upstream-gate.yml"))
listed = set(gate[True]["workflow_run"]["workflows"])
missing = []
for path in sorted(glob.glob(f"{dirpath}/*.yml")):
    wf = yaml.safe_load(open(path))
    on = wf.get(True) or wf.get("on")
    runs_on_pr = (on == "pull_request") or (isinstance(on, list) and "pull_request" in on) \
        or (isinstance(on, dict) and "pull_request" in on)
    if runs_on_pr and wf["name"] not in listed:
        missing.append(wf["name"])
print(", ".join(missing))
PY
)"
if [ -z "$wiring" ]; then
  echo "ok   - every pull_request workflow is listed in the gate's workflow_run trigger"
else
  echo "FAIL - workflows missing from sync-upstream-gate.yml workflow_run.workflows: $wiring"
  fails=$((fails + 1))
fi

if [ "$fails" -ne 0 ]; then
  echo "$fails test(s) failed"
  exit 1
fi
echo "all gate tests passed"
