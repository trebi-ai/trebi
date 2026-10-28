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
# A damaged memory: the Lessons section is missing. A file that only
# misses its Log entry raises no alert, so the gate needs a real fault.
dir=$(dirname "$(trebi job show digest --json | jq -r '.path')")
cat > "$dir/MEMORY.md" << 'EOF'
<!-- trebi memory v2. Keep these three sections in this order. Do not add other "## " headings. -->
# Memory

## Context

Daily digest.

## Log
EOF
