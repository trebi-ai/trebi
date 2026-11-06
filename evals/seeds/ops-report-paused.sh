#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-report.sh"
trebi job schedule agents/ops/report "0 7 * * *"
trebi job pause agents/ops/report
