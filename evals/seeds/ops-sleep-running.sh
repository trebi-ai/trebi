#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-sleep.sh"
trebi job run ops/sleeper || true
# The agent must find the run live, so wait for running, not a fixed delay.
for _ in $(seq 1 40); do
  n=$(trebi runs list --job ops/sleeper --status running --json | jq '(.runs // []) | length')
  [ "$n" -ge 1 ] && break
  sleep 1
done
