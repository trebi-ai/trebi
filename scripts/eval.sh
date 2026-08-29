#!/usr/bin/env bash
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"

mkdir -p build out

if [[ "$(uname -s)" == "Darwin" ]]; then
  export CGO_ENABLED=1
fi

go build -o build/trebi ./cmd/trebi
go build -o build/trebi-eval ./cmd/trebi-eval

export TREBI_BIN="$repo/build/trebi"
exec "$repo/build/trebi-eval" --evals "$repo/evals" --out "$repo/out" --bin "$TREBI_BIN" --skill "$repo/SKILL.md" "$@"
