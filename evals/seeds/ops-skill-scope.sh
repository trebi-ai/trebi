#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-cmo.sh"
cat > "$EVAL_WORKDIR/tone.md" << 'MD'
---
name: tone
description: The voice of all written output.
---
# Tone
Write in a formal voice.
MD
trebi skill create tone --file "$EVAL_WORKDIR/tone.md"
