#!/usr/bin/env bash
# Build and run trebi-eval. Does not exec, so EXIT always sweeps leftover
# trial daemons. Progress goes to stderr (unbuffered) and is teed to
# out/eval-live.log even when stdout is redirected.
#
#   scripts/eval.sh run --client codex
#   scripts/eval.sh run --client claude --trials 1 --task onboarding-health
#   EVAL_CLIENT=codex scripts/eval.sh run
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"

mkdir -p build out

if [[ "$(uname -s)" == "Darwin" ]]; then
  export CGO_ENABLED=1
fi

go build -o build/trebi ./cmd/trebi
go build -o build/trebi-eval ./cmd/trebi-eval

export TREBI_BIN="$repo/build/trebi"

log="${EVAL_LOG:-$repo/out/eval-live.log}"
mkdir -p "$(dirname "$log")"

sweep_eval_daemons() {
  local f pid
  while IFS= read -r -d '' f; do
    pid="$(tr -dc '0-9' < "$f" || true)"
    if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
      echo "sweep: killing leftover daemon pid ${pid} (${f})" >&2
      kill -TERM "${pid}" 2>/dev/null || true
      sleep 0.2
      kill -KILL -"${pid}" 2>/dev/null || true
      kill -KILL "${pid}" 2>/dev/null || true
    fi
  done < <(find "$repo/out" -name trebid.pid -print0 2>/dev/null || true)
}

trap sweep_eval_daemons EXIT INT TERM

sub=run
if [[ ${1:-} == run || ${1:-} == oracle || ${1:-} == review ]]; then
  sub=$1
  shift
fi

args=("$repo/build/trebi-eval"
  --evals "$repo/evals"
  --out "$repo/out"
  --bin "$TREBI_BIN"
  --skill "$repo/SKILL.md"
  "$sub")

if [[ "$sub" == run ]]; then
  has_client=0
  for a in "$@"; do
    case "$a" in
      --client|--client=*) has_client=1 ;;
    esac
  done
  if [[ $has_client -eq 0 && -n "${EVAL_CLIENT:-}" ]]; then
    args+=(--client "$EVAL_CLIENT")
  fi
fi
args+=("$@")

echo "eval log → ${log}" >&2
set +e
"${args[@]}" > >(tee -a "$log") 2> >(tee -a "$log" >&2)
st=$?
set -e
exit "$st"
