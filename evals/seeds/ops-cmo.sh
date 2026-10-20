#!/usr/bin/env bash
set -euo pipefail
. "${EVAL_SEEDS}/ops-assistant.sh"
trebi agent create cmo --harness codex --description "Growth plans and campaigns." --prompt "You run growth. Do what the job says."
