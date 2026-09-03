#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-sleep.sh"
trebi job run sleeper || true
sleep 8
RUN_ID=$(trebi runs list --job sleeper --json | jq -r '.runs[0].id // empty')
printf '%s' '{"question":"Overwrite the nightly report?"}' \
  | TREBI_RUN_ID="$RUN_ID" trebi agent-hook waiting > /dev/null
