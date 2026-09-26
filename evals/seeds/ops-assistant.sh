#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
cat > "$EVAL_WORKDIR/assistant-context.md" << 'MD'
You look after the ops workspace. Answer short.
MD
trebi assistant set --workspace ops --context-file "$EVAL_WORKDIR/assistant-context.md"
