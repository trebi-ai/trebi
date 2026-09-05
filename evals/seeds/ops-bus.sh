#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-report.sh"
trebi job validate report
# The task asks for run-topic events, so the bus must hold some. An unknown
# harness fails the launch fast and still emits run.created / run.updated /
# run.failed, so the seed does not wait on a real harness.
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Bus filler
harness: not-a-real-harness
---
Run the filler job.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" filler
set +e
trebi job run filler
set -e
sleep 2
