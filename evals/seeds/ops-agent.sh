#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
# A job lives in an agent folder, so the ops jobs live under ops/.
trebi agent create ops --harness codex --description "Runs the ops jobs." --prompt "You run the ops jobs. Do what the job says."
