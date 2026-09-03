#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-tick.sh"
# Drain holds the queue, so the row stays pending for the agent to cancel.
trebi service drain
printf '%s\n' '{"n":1}' > "$EVAL_WORKDIR/event.json"
trebi rules fire --integration tick --job report --match true --event-file "$EVAL_WORKDIR/event.json"
