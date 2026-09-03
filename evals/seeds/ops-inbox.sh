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

# Open a real approval on the first run, so the approval branch is live.
RUN_ID=$(trebi runs list --job nightly --json | jq -r '.runs[0].id // empty')
if [ -n "$RUN_ID" ]; then
  printf '%s' '{"question":"Overwrite the nightly report?"}' \
    | TREBI_RUN_ID="$RUN_ID" trebi agent-hook waiting > /dev/null
fi
