#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-report.sh"
cat > "$EVAL_WORKDIR/TREBI.md" << 'MD'
---
title: Ops assistant
harness: codex
---
You look after the ops workspace. Answer short.
MD
trebi assistant set --file "$EVAL_WORKDIR/TREBI.md"
