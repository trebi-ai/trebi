#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

mkdir -p build

# menuet tray needs cgo on macOS
if [[ "$(uname -s)" == "Darwin" ]]; then
  export CGO_ENABLED=1
fi

go build -o build/trebi ./cmd/trebi

echo "Built $(pwd)/build/trebi"

# Kick the LaunchAgent so it picks up the new binary when installed.
if launchctl list ai.trebi.daemon &>/dev/null; then
  echo "Restarting ai.trebi.daemon..."
  launchctl kickstart -k "gui/$UID/ai.trebi.daemon"
fi

# Symlink into PATH (and skill links when SKILL.md exists).
"$(dirname "$0")/register.sh"
