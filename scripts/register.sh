#!/usr/bin/env bash
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"

# Symlink the built binary into PATH. Skip if /usr/local/bin/trebi already
# resolves to the same file as our build (case-insensitive on macOS APFS),
# so re-running the script does not re-prompt for sudo.
target="$repo/build/trebi"
link="/usr/local/bin/trebi"
if [ ! -f "$target" ]; then
  echo "error: $target not found; run scripts/build.sh first" >&2
  exit 1
fi
if [ -e "$link" ] && [ "$link" -ef "$target" ]; then
  echo "ok: $link → $target (unchanged)"
else
  sudo ln -sfn "$target" "$link"
  echo "ok: $link → $target"
fi

# Skill doc: symlink into the Claude Code and Agents SDK skill trees so both
# see edits to the one file in this repo.
skill="$repo/SKILL.md"
if [ -f "$skill" ]; then
  for dir in "$HOME/.claude/skills/trebi" "$HOME/.agents/skills/trebi"; do
    mkdir -p "$dir"
    ln -sfn "$skill" "$dir/SKILL.md"
    echo "ok: $dir/SKILL.md → $skill"
  done
else
  echo "warn: $skill not found; skipped skill links" >&2
fi

# Warn if an interactive shell would still shadow PATH with an alias/function.
for rc in "$HOME/.bash_profile" "$HOME/.zshrc"; do
  if [ -f "$rc" ] && grep -E '^[[:space:]]*alias[[:space:]]+trebi=' "$rc" >/dev/null 2>&1; then
    echo "warn: $rc still defines alias trebi=… (shadows $link)" >&2
  fi
done
