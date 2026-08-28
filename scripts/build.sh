#!/usr/bin/env bash
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"

mkdir -p build

# menuet tray needs cgo on macOS
if [[ "$(uname -s)" == "Darwin" ]]; then
  export CGO_ENABLED=1
fi

go build -o build/trebi ./cmd/trebi

echo "Built $repo/build/trebi"

# Kick the LaunchAgent so it picks up the new binary when installed.
if launchctl list ai.trebi.daemon &>/dev/null; then
  echo "Restarting ai.trebi.daemon..."
  launchctl kickstart -k "gui/$UID/ai.trebi.daemon"
fi

# Symlink into PATH and refresh the skill links.
"$repo/scripts/register.sh"
