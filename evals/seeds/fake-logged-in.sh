#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
printf '1234\n' | trebi harness auth login fake
