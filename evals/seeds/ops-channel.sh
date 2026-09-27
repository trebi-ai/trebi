#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-assistant.sh"
# The workspace assistant answers a chat message, and its reply leaves the
# daemon through the connector channel into a file.
WSID=$(trebi workspace list --json | jq -r '.workspaces[] | select(.name == "ops") | .id')
cat > "$EVAL_WORKDIR/ops/trebi.yaml" <<YML
id: $WSID
name: ops
assistant:
  triggers:
    - event: chat
      match: "true"
YML
trebi connector set chat \
  --events-command "printf '%s\\n' '{\"chat_id\":\"eval-user\",\"text\":\"ping\"}'" \
  --format json --id chat_id --restart never \
  --channel-ref 'chat_id' \
  --send "cat >> \"$EVAL_WORKDIR/replies.txt\"" \
  --ack true
