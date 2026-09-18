#!/usr/bin/env bash
# Table test for semver-ge.sh (Part C of 2026-09-18-release-versioning.md).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/semver-ge.sh"
fails=0

check() { # want running minimum
  local want="$1" running="$2" minimum="$3" got=1
  if "$script" "$running" "$minimum" >/dev/null 2>&1; then
    got=0
  fi
  if [[ "$got" != "$want" ]]; then
    echo "FAIL semver-ge $running $minimum: want exit $want, got $got" >&2
    fails=$((fails + 1))
  fi
}

check 0 0.2.1 0.2.0   # newer patch
check 0 0.2.0 0.2.0   # equal
check 1 0.1.9 0.2.0   # older patch
check 0 v0.3.0 0.2.0  # leading v
check 0 0.2.1 v0.2.0  # leading v on the minimum
check 0 0.10.0 0.9.9  # numeric, not lexicographic
check 0 1.0.0 0.99.99 # major
check 1 0.2.0 0.3.0   # minor
check 0 0.2.0-3-gabc123-dirty 0.2.0 # git describe suffix is ignored
check 1 0.1.9-3-gabc123 0.2.0       # suffix cannot hide an old triple
check 0 dev 0.2.0     # dev builds skip the check
check 0 abc1234 0.2.0 # a bare SHA skips the check
check 0 0.2.0 abc     # an unparseable minimum skips the check
check 1 0.2.0+meta 0.2.1            # build metadata is ignored
check 0 0.2.1+meta 0.2.0

if ((fails > 0)); then
  exit 1
fi
echo "semver-ge tests ok"
