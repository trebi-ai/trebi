#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
cat > "$EVAL_WORKDIR/SKILL.md" << 'MD'
---
name: tiny
description: A tiny eval skill.
---
# Tiny
Say hello.
MD
trebi skill create --file "$EVAL_WORKDIR/SKILL.md" tiny
printf 'print("hi")\n' > "$EVAL_WORKDIR/helper.py"
trebi skill file put --from "$EVAL_WORKDIR/helper.py" tiny helper.py
