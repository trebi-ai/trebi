#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
REMOTE_HOME="$EVAL_WORKDIR/remote-home"
mkdir -p "$REMOTE_HOME"
TREBI_HOME="$REMOTE_HOME" "$TREBI_BIN" service daemon --listen 127.0.0.1:0 --no-tray >"$EVAL_WORKDIR/remote.log" 2>&1 &
echo $! > "$EVAL_WORKDIR/remote.pid"
for _ in $(seq 1 50); do
  if [[ -f "$REMOTE_HOME/api.json" ]]; then
    break
  fi
  sleep 0.1
done
URL=$(jq -r .url "$REMOTE_HOME/api.json")
TREBI_HOME="$REMOTE_HOME" "$TREBI_BIN" workspace create --name other --mkdir --use "$EVAL_WORKDIR/remote-other"
WS_ID=$(TREBI_HOME="$REMOTE_HOME" "$TREBI_BIN" workspace list --json | jq -r '.workspaces[] | select(.name == "other") | .id')
cat > "$EVAL_WORKDIR/REMOTE.md" << 'EOF'
---
title: Ping
harness: codex
---
Say ping.
EOF
TREBI_HOME="$REMOTE_HOME" "$TREBI_BIN" job create --file "$EVAL_WORKDIR/REMOTE.md" ping
printf '%s\n' "$URL" > "$EVAL_WORKDIR/remote.url"
printf '%s\n' "$WS_ID" > "$EVAL_WORKDIR/remote.workspace"
