#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Digest
harness: fake
memory: true
---
Print the digest line and stop.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" digest
trebi job validate digest
