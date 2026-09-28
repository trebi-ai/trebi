#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-report.sh"
trebi job create --file "$EVAL_WORKDIR/JOB.md" ingest
cat > "$EVAL_WORKDIR/tick.yaml" <<'YML'
tick:
  from: inline
  manifest:
    schema: trebi-connector/1
    name: tick
    title: Tick
    description: Prints one tick event.
    version: 1.0.0
    events:
      protocol: lines
      command: printf '%s\n' '{"n":1}'
      format: json
      restart: never
YML
trebi connector apply --file "$EVAL_WORKDIR/tick.yaml"
