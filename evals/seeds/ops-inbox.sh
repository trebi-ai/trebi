#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
for name in nightly other; do
  cat > "$EVAL_WORKDIR/JOB.md" << EOF
---
title: $name
harness: not-a-real-harness
---
Run $name.
EOF
  trebi job create --file "$EVAL_WORKDIR/JOB.md" "$name"
  set +e
  trebi job run "$name"
  set -e
done
sleep 2
