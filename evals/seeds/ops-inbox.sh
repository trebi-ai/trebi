#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-agent.sh"
# A failed job wakes its agent two times before it raises run_failed.
# The fake harness keeps those wakes cheap.
trebi agent patch ops harness=fake
for name in nightly other; do
  cat > "$EVAL_WORKDIR/JOB.md" << EOF
---
title: $name
harness: not-a-real-harness
---
Run $name.
EOF
  trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent ops "$name"
  set +e
  for _ in 1 2 3; do
    trebi job run "ops/$name" --wait --timeout 1m
    sleep 2
  done
  set -e
done
sleep 2

# Open a real approval on the first run, so the approval branch is live.
RUN_ID=$(trebi runs list --job ops/nightly --json | jq -r '.runs[0].id // empty')
if [ -n "$RUN_ID" ]; then
  printf '%s' '{"question":"Overwrite the nightly report?"}' \
    | TREBI_RUN_ID="$RUN_ID" trebi agent-hook waiting > /dev/null
fi
