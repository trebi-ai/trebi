#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Nightly
harness: not-a-real-harness
---
Run the nightly job.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" nightly
set +e
trebi job run nightly
set -e
# launch fails fast on an unknown harness; give the inbox/alerts a moment
sleep 2
