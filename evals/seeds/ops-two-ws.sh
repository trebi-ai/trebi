#!/usr/bin/env bash
set -euo pipefail
trebi workspace add --name ops --create --use "$EVAL_WORKDIR/ops"
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Report
harness: codex
---
Write staging-or-ops to out.txt and stop.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" report
trebi workspace add --name staging --create --use "$EVAL_WORKDIR/staging"
trebi job create --file "$EVAL_WORKDIR/JOB.md" report
trebi workspace use ops
