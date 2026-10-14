#!/usr/bin/env bash
# Live resume tier: one run keeps the harness context across turns.
#
# Runs TestLiveResume (go test -tags live ./internal/runs) against the REAL
# vendor CLIs with the existing shared login. Each harness gets two turns in
# one run and must recall a code word. Real prompts spend a small amount of quota.
#
#   scripts/test.resume.sh                  # every installed harness, CLI runtime
#   scripts/test.resume.sh claude codex     # some harnesses
#   scripts/test.resume.sh --tui codex      # the tmux runtime
#   scripts/test.resume.sh --restart claude # stop the daemon mid-turn (SIGTERM, SIGKILL), then restart it
#
# Results go to $TREBI_LIVE_OUT (default build/live-resume.txt or build/live-restart.txt), one line per case.
# TREBI_LIVE_SIGNALS (default term,kill) limits the --restart signals.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"

runtime=cli
test=TestLiveResume
out=live-resume.txt
named=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tui)         runtime=tui; shift ;;
    --restart)     test=TestLiveRestart; out=live-restart.txt; shift ;;
    --skip-noauth) export TREBI_LIVE_SKIP_NOAUTH=1; shift ;;
    -h|--help)     sed -n '2,14p' "$0"; exit 0 ;;
    -*)            echo "error: unknown flag $1" >&2; exit 2 ;;
    *)             named+=("$1"); shift ;;
  esac
done

export TREBI_LIVE_RUNTIME="$runtime"
if [[ ${#named[@]} -gt 0 ]]; then
  TREBI_LIVE_HARNESSES="$(IFS=,; echo "${named[*]}")"
  export TREBI_LIVE_HARNESSES
fi
export TREBI_LIVE_OUT="${TREBI_LIVE_OUT:-$repo/build/$out}"
mkdir -p "$(dirname "$TREBI_LIVE_OUT")"

args=(go test -tags live -count=1 -timeout 40m -v -run "^$test\$" ./internal/runs)
echo "+ ${args[*]}"
status=0
"${args[@]}" || status=$?
echo "results: $TREBI_LIVE_OUT"
cat "$TREBI_LIVE_OUT" 2>/dev/null || true
exit "$status"
