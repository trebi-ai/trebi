#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

# Symlink the built binary into PATH. Skip if /usr/local/bin/trebi already
# resolves to the same file as our build (case-insensitive on macOS APFS),
# so re-running the script does not re-prompt for sudo.
target="$(pwd)/build/trebi"
link="/usr/local/bin/trebi"
if [ ! -f "$target" ]; then
  echo "error: $target not found; run scripts/build.sh first" >&2
  exit 1
fi
if [ -e "$link" ] && [ "$link" -ef "$target" ]; then
  echo "ok: $link → $target (unchanged)"
else
  sudo ln -sf "$target" "$link"
  echo "ok: $link → $target"
fi

# Optional skill doc (when present at trebi/SKILL.md): hard-link into Claude
# Code and Agents SDK skill dirs so either tree sees edits.
if [ -f SKILL.md ]; then
  mkdir -p "$HOME/.claude/skills/trebi"
  ln -f SKILL.md "$HOME/.claude/skills/trebi/SKILL.md"
  mkdir -p "$HOME/.agents/skills/trebi"
  ln -f SKILL.md "$HOME/.agents/skills/trebi/SKILL.md"
  echo "ok: skill hard-links refreshed"
fi

# Warn if an interactive shell would still shadow PATH with an alias/function.
if [ -f "$HOME/.bash_profile" ] && grep -E '^[[:space:]]*alias[[:space:]]+trebi=' "$HOME/.bash_profile" >/dev/null 2>&1; then
  echo "warn: ~/.bash_profile still defines alias trebi=… (shadows $link)" >&2
fi
if [ -f "$HOME/.zshrc" ] && grep -E '^[[:space:]]*alias[[:space:]]+trebi=' "$HOME/.zshrc" >/dev/null 2>&1; then
  echo "warn: ~/.zshrc still defines alias trebi=… (shadows $link)" >&2
fi
