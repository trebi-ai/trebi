#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-cmo.sh"
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Weekly digest
---
Draft the weekly digest.
EOF
trebi file put agents/cmo/JOB.md --from "$EVAL_WORKDIR/JOB.md"
