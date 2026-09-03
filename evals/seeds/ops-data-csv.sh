#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
printf 'a,b\n1,2\n' > "$EVAL_WORKDIR/data.csv"
trebi file put --from "$EVAL_WORKDIR/data.csv" data.csv
