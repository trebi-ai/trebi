#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-agent.sh"
cat > "$EVAL_WORKDIR/loop.sh" << 'EOF'
#!/bin/sh
while true; do sleep 2; done
EOF
chmod +x "$EVAL_WORKDIR/loop.sh"
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Sleeper
harness: codex
session:
  max_duration: 30m
---
Run ./loop.sh in the job folder with `sh ./loop.sh` and leave it in the
foreground. It never returns. Do not background it, do not stop it, and do not
exit or summarise while it runs.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent ops sleeper
trebi job file put --from "$EVAL_WORKDIR/loop.sh" agents/ops/sleeper loop.sh
