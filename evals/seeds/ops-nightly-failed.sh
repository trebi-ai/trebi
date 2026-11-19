#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-agent.sh"
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF2'
---
title: Nightly
harness: not-a-real-harness
---
Write nightly.txt with one line, then stop.
EOF2
trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent ops nightly
trebi job run ops/nightly || true
sleep 2
# Repair the job so a plain retry is enough. The failed run stays on record.
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF2'
---
title: Nightly
harness: codex
---
Write nightly.txt with one line, then stop.
EOF2
trebi job set --file "$EVAL_WORKDIR/JOB.md" ops/nightly
