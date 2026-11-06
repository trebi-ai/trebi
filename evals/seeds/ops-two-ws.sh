#!/usr/bin/env bash
set -euo pipefail
trebi workspace create --name ops --mkdir --use "$EVAL_WORKDIR/ops"
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF2'
---
title: Report
harness: codex
---
Write staging-or-ops to out.txt and stop.
EOF2
# Each workspace has its own ops agent, so both report jobs have the same name.
trebi agent create ops --harness codex --description "Runs the ops jobs." --prompt "You run the ops jobs. Do what the job says."
trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent ops report
trebi workspace create --name staging --mkdir --use "$EVAL_WORKDIR/staging"
trebi agent create ops --harness codex --description "Runs the ops jobs." --prompt "You run the ops jobs. Do what the job says."
trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent ops report
trebi workspace use ops
