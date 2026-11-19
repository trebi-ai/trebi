#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-agent.sh"
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Nightly
harness: not-a-real-harness
---
Run the nightly job.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent ops nightly
set +e
trebi job run ops/nightly
set -e
# launch fails fast on an unknown harness; give the inbox/alerts a moment
sleep 2
