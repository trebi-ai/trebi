#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
# Stamp the window before the failures, so every seeded alert is inside it.
date -u +%Y-%m-%dT%H:%M:%SZ > "$EVAL_WORKDIR/alerts-since.txt"
sleep 1
for name in nightly other; do
  cat > "$EVAL_WORKDIR/JOB.md" << JOB
---
title: $name
harness: not-a-real-harness
---
Run $name.
JOB
  trebi job create --file "$EVAL_WORKDIR/JOB.md" "$name"
  set +e
  trebi job run "$name"
  set -e
done
sleep 2
