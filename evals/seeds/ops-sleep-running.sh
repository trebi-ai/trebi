#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-sleep.sh"
trebi job run sleeper || true
# Give the launch time to reach running before the agent starts.
sleep 8
