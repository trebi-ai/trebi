#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-agent.sh"
# A failed job wakes its agent two times before it raises run_failed.
# The fake harness keeps those wakes cheap.
trebi agent patch ops harness=fake
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
  trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent ops "$name"
  set +e
  for _ in 1 2 3; do
    trebi job run "ops/$name" --wait --timeout 1m
    sleep 2
  done
  set -e
done
sleep 2
