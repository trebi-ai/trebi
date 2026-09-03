#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-report.sh"
trebi job schedule report "0 7 * * *"
trebi job pause report
