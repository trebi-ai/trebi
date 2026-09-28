#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-tick.sh"
# The fetch tool can replay history. The cursor sits at event 1, so a restart
# replays events 2 and 3 through events.catchup.command.
cat > "$EVAL_WORKDIR/tick-catchup.yaml" <<'YML'
events:
  id: n
  catchup:
    command: printf '%s\n' '{"n":1}' '{"n":2}' '{"n":3}'
YML
trebi connector set tick --patch-file "$EVAL_WORKDIR/tick-catchup.yaml"
trebi connector cursor set tick 1
