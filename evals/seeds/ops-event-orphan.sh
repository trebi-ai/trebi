#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-tick.sh"
# Record an event while ingest has no trigger, so it starts no run.
printf '%s\n' '{"n":1}' > "$EVAL_WORKDIR/event.json"
trebi rules fire --integration tick --match true --event-file "$EVAL_WORKDIR/event.json"
# Now wire ingest, so a retrigger of that event does start a run.
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF2'
---
title: Ingest
harness: codex
triggers:
  - event: tick
    match: "true"
---
Ingest the event and stop.
EOF2
trebi job set --file "$EVAL_WORKDIR/JOB.md" ingest
trebi job validate ingest
