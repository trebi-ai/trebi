#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-inbox.sh"
date -u +%Y-%m-%dT%H:%M:%SZ > "$EVAL_WORKDIR/alerts-since.txt"
