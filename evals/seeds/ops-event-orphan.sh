#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-tick.sh"
# Record an event while ingest has no route, so it starts no run.
printf '%s\n' '{"n":1}' > "$EVAL_WORKDIR/event.json"
trebi rules fire --connector tick --match true --event-file "$EVAL_WORKDIR/event.json"
# Now route tick to ingest, so a retrigger of that event does start a run.
mkdir -p "$EVAL_WORKDIR/ops/routes"
cat > "$EVAL_WORKDIR/ops/routes/tick.yaml" << 'EOF2'
routes:
  - job: ops/ingest
    gets: every_message
    match: "true"
EOF2
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF2'
---
title: Ingest
harness: codex
---
Ingest the event and stop.
EOF2
trebi job set --file "$EVAL_WORKDIR/JOB.md" agents/ops/ingest
trebi job validate agents/ops/ingest
