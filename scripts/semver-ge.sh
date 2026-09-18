#!/usr/bin/env bash
# semver-ge.sh <version> <min>: exit 0 when <version> satisfies <min>.
#
# The release comparison rule (Part C of 2026-09-18-release-versioning.md):
# strip a leading "v", compare the major.minor.patch triple numerically, and
# ignore any prerelease or build suffix. When <version> does not parse (a dev
# daemon reports "dev" or a git SHA), the check passes; the rule exists so a
# dev binary never trips the compatibility gate.
set -euo pipefail

running="${1:-}"
minimum="${2:-}"

core() {
  local v="${1#v}"
  v="${v%%-*}"
  v="${v%%+*}"
  if [[ "$v" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    printf '%s %s %s' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}"
    return 0
  fi
  return 1
}

if ! min_core="$(core "$minimum")"; then
  echo "semver-ge: cannot parse minimum version '$minimum'; skipping the check" >&2
  exit 0
fi

if ! running_core="$(core "$running")"; then
  echo "semver-ge: '$running' is not a release version; treating it as compatible" >&2
  exit 0
fi

read -r r_major r_minor r_patch <<<"$running_core"
read -r m_major m_minor m_patch <<<"$min_core"

if ((r_major != m_major)); then
  ((r_major > m_major)) && exit 0
  exit 1
fi
if ((r_minor != m_minor)); then
  ((r_minor > m_minor)) && exit 0
  exit 1
fi
if ((r_patch >= m_patch)); then
  exit 0
fi
echo "semver-ge: $running is older than $minimum" >&2
exit 1
