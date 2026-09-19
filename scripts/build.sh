#!/usr/bin/env bash
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"

with_ui=1
for arg in "$@"; do
  case "$arg" in
    --no-ui) with_ui=0 ;;
    *)
      echo "unknown flag: $arg (usage: build.sh [--no-ui])" >&2
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
  # Local builds embed the working tree of the sibling trebi-ui checkout, not
  # a release tarball (only the release workflow downloads those). Override
  # the checkout with TREBI_UI_SRC; without a checkout, fall back to whatever
  # is already staged in internal/ui/dist.
  ui_src="${TREBI_UI_SRC:-$repo/../trebi-ui}"
  if [[ -n "${TREBI_UI_SRC:-}" && ! -d "$ui_src/apps/app" ]]; then
    echo "TREBI_UI_SRC=$TREBI_UI_SRC is not a trebi-ui checkout" >&2
    exit 1
  fi
  if [[ -d "$ui_src/apps/app" ]]; then
    if [[ ! -d "$ui_src/node_modules" ]]; then
      echo "$ui_src has no node_modules; run 'pnpm install' there first" >&2
      exit 1
    fi
    # Stamp the local UI the way the backend stamps itself: nearest tag
    # first, short SHA otherwise. The release job ignores this and uses its
    # npm-stamped package.json version; TREBI_UI_VERSION overrides both.
    ui_build_version="${TREBI_UI_VERSION:-}"
    if [[ -z "$ui_build_version" ]]; then
      ui_build_version="$(git -C "$ui_src" describe --tags --always --dirty 2>/dev/null || true)"
      ui_build_version="${ui_build_version#v}"
    fi
    echo "Building the UI from $ui_src"
    (cd "$ui_src" && TREBI_UI_VERSION="$ui_build_version" pnpm --filter @trebi/app build)
    rm -rf "$repo/internal/ui/dist"
    mkdir -p "$repo/internal/ui/dist"
    cp -R "$ui_src/apps/app/dist/." "$repo/internal/ui/dist/"
  fi

  dist="$repo/internal/ui/dist"
  if [[ ! -f "$dist/index.html" ]]; then
    echo "no UI build found at $dist; set TREBI_UI_SRC to a trebi-ui checkout, or pass --no-ui" >&2
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
else
  built="$built, no ui"
fi
echo "Built $repo/build/trebi $built)"

# Kick the LaunchAgent so it picks up the new binary when installed.
if launchctl list ai.trebi.daemon &>/dev/null; then
  echo "Restarting ai.trebi.daemon..."
  launchctl kickstart -k "gui/$UID/ai.trebi.daemon"
fi

# Symlink into PATH and refresh the skill links.
"$repo/scripts/register.sh"
