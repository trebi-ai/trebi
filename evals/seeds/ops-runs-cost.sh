#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-report.sh"
cat > "$EVAL_WORKDIR/JOB.md" << 'JOB'
---
title: Digest
harness: codex
---
Write a one-line digest to digest.txt in this job folder, then stop.
JOB
trebi job create --file "$EVAL_WORKDIR/JOB.md" digest
# Real runs, so runs stats has a real cost to report. Two is enough.
trebi job run report --wait 120000 || true
trebi job run digest --wait 120000 || true
