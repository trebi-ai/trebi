#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
for pair in "alpha:claude" "beta:codex" "gamma:cursor"; do
  name=${pair%%:*}
  harness=${pair##*:}
  cat > "$EVAL_WORKDIR/JOB.md" << EOF2
---
title: $name
harness: $harness
---
Write $name.txt with one line, then stop.
EOF2
  trebi job create --file "$EVAL_WORKDIR/JOB.md" "$name"
done
