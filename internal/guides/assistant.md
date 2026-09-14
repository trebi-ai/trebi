---
topic: assistant
title: Assistant, conversations, and memory
description: Wake, say, conversations, and the assistant memory ledger.
---

# Assistant, conversations, and memory

The assistant is the workspace singleton driven by `TREBI.md`. When `assistant.model` is set and an API key resolves, the daemon runs it in-process (`harness=native`) — no vendor CLI, no tmux pane, no attach. `runtime: tui` in TREBI.md is the escape hatch back to the old harness. Native is API-key only (`assistant.api_key` / `assistant.api_key_env`).

## Drive the assistant

Pass `--workspace` on the root (`trebi --workspace reports assistant …`) or use sticky context (`workspace use`, **local only**).

```bash
trebi --workspace reports assistant show
trebi --workspace reports assistant wake
trebi --workspace reports assistant wake --test
trebi --workspace reports assistant wake --extra-prompt "focus on the CI failures"
trebi --workspace reports assistant say "what is blocked?"
trebi --workspace reports assistant say "what is blocked?" --wait 60000 --json
```

`assistant say` prints `delivery=injected|queued`. The daemon resolves or creates the conversation, so no pointer call is needed first. `--wait` long-polls for the reply, up to 120000 ms; `replies` carries what arrived. `--test` evaluates gates only (same rule as `job run --test`).

```bash
trebi --workspace reports assistant wake --attach
trebi assistant chat
```

`assistant chat` and `--attach` are **local only**; remote: `assistant.say` plus `conversation.messages`. Never attach from an agent session.

```bash
trebi assistant history --limit 20 --json
```

`assistant history` is **local only**; remote: `conversation list` / `conversation messages`.

## Conversations

Conversations are the chat object. The CLI channel pointer is per workspace.

```bash
trebi conversation list --json --limit 20
trebi conversation show <id> --json
trebi conversation messages <id> --json --limit 50
trebi conversation new --title "CI firefight"
trebi conversation switch <id>
```

`conversation switch` is **local only** (sticky CLI pointer); remote: pass `channel_ref` on `assistant.say`, or `pointer_set`.

Use `assistant say --wait`, or poll `conversation messages`, until the assistant replies. Do not `runs kill` a live assistant session to re-prompt.

## Assistant memory

The assistant keeps a structured `MEMORY.md` at the workspace root. It holds four sections in a fixed order: Context, State, Lessons, Log. It uses the config defaults only; it reads no frontmatter.

```bash
trebi --workspace reports assistant memory show --json
trebi --workspace reports assistant memory validate
trebi --workspace reports assistant memory set-state --body state.md
trebi --workspace reports assistant memory lesson add --text "Reviewed the backlog"
trebi --workspace reports assistant memory log add --text "planned the week"
trebi --workspace reports assistant memory log delete <entry-id>
trebi --workspace reports assistant memory clear --force
```

`--body` and `--file` are **local only**; remote: pass `body` / `content`. `memory clear` is destructive — ask first. Offline repair: `trebi memory validate <path> --fix`.

Job memory (per job, not the assistant): `trebi guide jobs-and-scheduling`. Folder-scoped agents (nested operators with their own folders): `trebi guide agents`.
