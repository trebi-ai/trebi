#!/usr/bin/env bash
# trebi Codex notify wrapper. Codex invokes notify as a program with a JSON
# payload on stdin (agent-turn-complete). We soft-publish stop so AgentBus
# updates idle without promoting to full event mode (screen still drives busy).
set -eu
if [ -z "${TREBI_RUN_ID:-}" ]; then
  exit 0
fi
# Soft event: agent-hook stop-soft (daemon maps without event-mode promotion).
exec trebi agent-hook stop-soft
