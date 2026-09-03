#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-secret.sh"
trebi secret set --from-file "$EVAL_WORKDIR/api-key.txt" eval_api_key
