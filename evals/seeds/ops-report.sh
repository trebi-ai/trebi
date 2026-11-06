#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-agent.sh"
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Report
harness: codex
---
Write a one-line status report to report.txt in this job folder, then stop.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent ops report
trebi job validate agents/ops/report
