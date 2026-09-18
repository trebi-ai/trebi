#!/usr/bin/env bash
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"

with_ui=0
for arg in "$@"; do
  case "$arg" in
    --with-ui) with_ui=1 ;;
    *)
      echo "unknown flag: $arg (usage: build.sh [--with-ui])" >&2
      exit 1
      ;;
  esac
done

mkdir -p build

# Live-tier compile gate (docs/plans/2026-09-16-harness-subprocess-runtime.md
# Part L.1): build-tagged live tests must keep compiling even when not run.
go test -tags live_cli -run '^$' ./internal/runtime/cli/...

# Version is a git fact: a tag when one is reachable, a short SHA otherwise.
# The leading v is stripped so health reports the same shape the release
# stamping uses; anything after the major.minor.patch triple is ignored by the
# release rule (Part C).
version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
version="${version#v}"
ldflags="-X github.com/trebi-ai/trebi/internal/api.Version=$version"
tags=()

if [[ "$with_ui" == 1 ]]; then
  dist="$repo/internal/ui/dist"
  if [[ ! -f "$dist/index.html" ]]; then
    echo "no UI build at $dist; run trebi-ui 'pnpm build:embed' first" >&2
    exit 1
  fi
  ui_version=""
  if [[ -f "$dist/manifest.json" ]]; then
    ui_version="$(sed -n 's/.*"ui_version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
      "$dist/manifest.json")"
    ui_version="${ui_version%%$'\n'*}"
  fi
  ldflags="$ldflags -X github.com/trebi-ai/trebi/internal/api.UIVersion=$ui_version"
  tags=(-tags ui)
fi

# menuet tray needs cgo on macOS
if [[ "$(uname -s)" == "Darwin" ]]; then
  export CGO_ENABLED=1
fi

go build ${tags[@]+"${tags[@]}"} -ldflags "$ldflags" -o build/trebi ./cmd/trebi

built="($version"
if [[ "$with_ui" == 1 ]]; then
  built="$built, ui ${ui_version:-unknown}"
fi
echo "Built $repo/build/trebi $built)"

# Kick the LaunchAgent so it picks up the new binary when installed.
if launchctl list ai.trebi.daemon &>/dev/null; then
  echo "Restarting ai.trebi.daemon..."
  launchctl kickstart -k "gui/$UID/ai.trebi.daemon"
fi

# Symlink into PATH and refresh the skill links.
"$repo/scripts/register.sh"
