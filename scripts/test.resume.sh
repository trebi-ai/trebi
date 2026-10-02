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
#
# Results go to $TREBI_LIVE_OUT (default build/live-resume.txt), one line per harness and runtime.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"

runtime=cli
named=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tui)         runtime=tui; shift ;;
    --skip-noauth) export TREBI_LIVE_SKIP_NOAUTH=1; shift ;;
    -h|--help)     sed -n '2,12p' "$0"; exit 0 ;;
    -*)            echo "error: unknown flag $1" >&2; exit 2 ;;
    *)             named+=("$1"); shift ;;
  esac
done

export TREBI_LIVE_RUNTIME="$runtime"
if [[ ${#named[@]} -gt 0 ]]; then
  TREBI_LIVE_HARNESSES="$(IFS=,; echo "${named[*]}")"
  export TREBI_LIVE_HARNESSES
fi
export TREBI_LIVE_OUT="${TREBI_LIVE_OUT:-$repo/build/live-resume.txt}"
mkdir -p "$(dirname "$TREBI_LIVE_OUT")"

args=(go test -tags live -count=1 -timeout 40m -v -run '^TestLiveResume$' ./internal/runs)
echo "+ ${args[*]}"
status=0
"${args[@]}" || status=$?
echo "results: $TREBI_LIVE_OUT"
cat "$TREBI_LIVE_OUT" 2>/dev/null || true
exit "$status"
