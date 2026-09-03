#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
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
  max_duration: 2m
---
Run ./loop.sh in the workspace folder and keep it running. Do not exit.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" sleeper
trebi job file put --from "$EVAL_WORKDIR/loop.sh" sleeper loop.sh
