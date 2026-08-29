#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops.sh"
# Distinctive 404 so the agent can grep daemon logs.
set +e
trebi job show eval-log-canary-xyzzy
set -e
true
