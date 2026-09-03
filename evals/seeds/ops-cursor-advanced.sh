#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-tick.sh"
printf '%s\n' '{"n":1}' > "$EVAL_WORKDIR/event.json"
trebi rules fire --integration tick --match true --event-file "$EVAL_WORKDIR/event.json"
# Park the cursor past every stored event, so replay needs a reset.
LAST=$(trebi event list --integration tick --json | jq -r '.events[0].id')
trebi integration cursor set tick "$LAST"
