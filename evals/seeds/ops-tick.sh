#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-report.sh"
trebi job create --file "$EVAL_WORKDIR/JOB.md" ingest
trebi connector set tick --events-command "printf '%s\\n' '{\"n\":1}'" --format json --restart never
