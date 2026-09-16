#!/usr/bin/env bash
# Live tier for the harness subprocess runtime (docs/plans/2026-09-16-harness-subprocess-runtime.md, Part L.2).
#
# Runs the REAL vendor CLIs against the operator's existing shared login:
# auth is asserted before any prompt is sent, nothing logs in or out, and
# there are no mocks. This consumes a small amount of plan quota.
#
#   scripts/test.live.sh                        # every installed harness
#   scripts/test.live.sh claude                 # one harness
#   scripts/test.live.sh claude codex           # several
#   scripts/test.live.sh --run TestLiveClaudePermission claude
#
# Env:
#   TREBI_LIVE_SKIP_NOAUTH=1   downgrade auth failures to skips (mixed machines)
#
# The tests set a temp TREBI_HOME themselves; the shared login still resolves
# through the user home / Keychain.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"

pkg=internal/runtime/cli
go_timeout=20m
run_re=""
dry_run=0
assume_yes=0
skip_noauth=0
named=()
extra=()

known=(claude codex opencode cursor pi)

binary_for() {
  case "$1" in
    cursor) echo cursor-agent ;;
    *)      echo "$1" ;;
  esac
}

usage() {
  cat <<'EOF'
usage: scripts/test.live.sh [flags] [harness ...]

Runs the live tier (go test -tags live_cli) against installed vendor CLIs
using your existing shared login. With no harness names, runs every
installed harness among: claude codex opencode cursor pi.
Note: an installed-but-unauthenticated harness fails unless --skip-noauth.

flags:
  --run <regex>     pass -run to go test
  --timeout <dur>   go test timeout (default 20m)
  --skip-noauth     skip harnesses whose login is invalid instead of failing
  --dry-run         print the plan and exit without running
  -y, --yes         do not prompt before spending quota
  -h, --help        this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)     usage; exit 0 ;;
    --run)         run_re="${2:?--run needs a regex}"; shift 2 ;;
    --timeout)     go_timeout="${2:?--timeout needs a value}"; shift 2 ;;
    --skip-noauth) skip_noauth=1; shift ;;
    --dry-run)     dry_run=1; shift ;;
    -y|--yes)      assume_yes=1; shift ;;
    --)            shift; extra=("$@"); break ;;
    -*)            echo "error: unknown flag $1" >&2; usage >&2; exit 2 ;;
    *)             named+=("$1"); shift ;;
  esac
done

if [[ ! -d "$pkg" ]]; then
  echo "error: $pkg does not exist yet (phase 0 of the plan); nothing to test" >&2
  exit 1
fi
if ! compgen -G "$pkg/live_*_test.go" >/dev/null; then
  echo "error: no $pkg/live_*_test.go yet; the live tier (Part L.2) is not implemented" >&2
  exit 1
fi

harnesses=()
if [[ ${#named[@]} -gt 0 ]]; then
  harnesses=("${named[@]}")
else
  for h in "${known[@]}"; do
    if command -v "$(binary_for "$h")" >/dev/null 2>&1; then
      harnesses+=("$h")
    fi
  done
fi
if [[ ${#harnesses[@]} -eq 0 ]]; then
  echo "error: no vendor CLIs found on PATH (looked for: ${known[*]})" >&2
  exit 1
fi

echo "live harnesses: ${harnesses[*]}"
for h in "${harnesses[@]}"; do
  bin="$(binary_for "$h")"
  if command -v "$bin" >/dev/null 2>&1; then
    echo "  $h -> $(command -v "$bin")"
  else
    echo "  $h -> MISSING ($bin not on PATH; the live test will fail as requested)"
  fi
done
echo "timeout: $go_timeout"
echo "note: real prompts run against your logged-in accounts and consume plan quota."

if [[ $dry_run -eq 1 ]]; then
  echo "dry run: not executing"
  exit 0
fi

if [[ $assume_yes -eq 0 && -t 0 ]]; then
  read -r -p "Continue? [y/N] " ans
  case "$ans" in
    y|Y|yes|YES) ;;
    *) echo "aborted"; exit 130 ;;
  esac
fi

TREBI_LIVE_HARNESSES="$(IFS=,; echo "${harnesses[*]}")"
export TREBI_LIVE_HARNESSES
if [[ $skip_noauth -eq 1 ]]; then
  export TREBI_LIVE_SKIP_NOAUTH=1
fi

args=(go test -tags live_cli -count=1 -timeout "$go_timeout" -v "./$pkg/...")
if [[ -n "$run_re" ]]; then
  args+=(-run "$run_re")
fi
if [[ ${#extra[@]} -gt 0 ]]; then
  args+=("${extra[@]}")
fi

echo "+ ${args[*]}"
exec "${args[@]}"
