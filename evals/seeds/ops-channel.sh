#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-assistant.sh"
# The workspace assistant answers a chat message. The reply leaves the daemon
# as a messages/send request to a sandbox adapter, which records it in a file.
WSID=$(trebi workspace list --json | jq -r '.workspaces[] | select(.name == "ops") | .id')
cat > "$EVAL_WORKDIR/ops/trebi.yaml" <<YML
id: $WSID
name: ops
assistant:
  triggers:
    - event: chat
      match: "true"
YML
cat > "$EVAL_WORKDIR/chat-sandbox.json" <<JSON
{"result": {"features": []}, "record": "$EVAL_WORKDIR/replies.txt"}
JSON
FAKE=$(command -v fake)
cat > "$EVAL_WORKDIR/chat.yaml" <<YML
type: chat
slug: chat
from: inline
inputs:
  TREBI_FAKEADAPTER: sandbox:$EVAL_WORKDIR/chat-sandbox.json
manifest:
  schema: trebi-connector/1
  name: chat
  title: Chat
  description: A sandbox chat channel for the eval.
  version: 1.0.0
  events:
    protocol: trebi-connector/1
    command: '"$FAKE" connector'
    restart: never
    types:
      - type: message
  channel: {}
YML
trebi connector apply --file "$EVAL_WORKDIR/chat.yaml"
