#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-report.sh"
trebi job create --file "$EVAL_WORKDIR/JOB.md" ingest
trebi integration set tick --type cli --format json -- printf '%s\n' '{"n":1}'
