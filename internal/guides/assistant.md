---
topic: assistant
title: Assistant, conversations, and memory
description: Assistant settings, chat sessions, conversations, and the memory ledger.
---

# Assistant, conversations, and memory

The assistant is a chat agent for one workspace. It keeps one live agent session per conversation and closes the process when the conversation is idle. The assistant is not a job and has no runs.

## Settings

Settings live in two layers. The global defaults are in `~/.trebi/config.yaml` under `assistant:`. Each workspace can override fields in `<workspace>/trebi.yaml` under `assistant:`. An absent key inherits the default.

```bash
trebi assistant show --workspace reports
trebi assistant show --defaults
trebi assistant set --workspace reports --harness codex --mode auto_edit
trebi assistant set --defaults --skill trebi --skill sling
trebi assistant set --workspace reports --unset harness
trebi assistant preview --workspace reports
```

`harness` picks `native` or an installed CLI harness; empty means auto. `mode` is `ask`, `auto_edit`, or `auto`. `context-file` points at a file with the workspace context prompt. `--unset` removes a workspace override, so the field inherits again.

`preview` prints the instructions a new session gets: role, workspace, context, and memory.

## Chat

`say` sends one message and returns. `chat` is interactive: replies stream as they form, tool calls print as one line each, and permission requests ask for `y` (allow once), `a` (allow for the session), or `n` (deny).

```bash
trebi --workspace reports assistant say "what is blocked?"
trebi --workspace reports assistant say "what is blocked?" --wait 60000 --json
trebi assistant chat
```

`chat` is **local only**; remote: `assistant.say` plus `conversation.messages`. Never drive chat from an agent session; use `agent escalate` instead.

## Sessions

Each conversation has a session status: `starting`, `running`, `waiting`, `idle`, `closed`, or `error`.

```bash
trebi conversation list --json
trebi assistant interrupt <conversation id>
trebi assistant close <conversation id>
trebi conversation delete <conversation id>
```

`close` is **local only**; remote: `conversation.interrupt` then let the idle close reap the process.

`sessions` is **local only**: it lists the assistant conversations of a workspace. `interrupt` stops the running turn. `close` is **local only**: it closes the process; the next prompt starts it again with the earlier context. `conversation delete` removes the conversation with its messages. Do not `runs kill` to stop an assistant; that is a job action.

## Conversations

Conversations are the chat object. The CLI channel pointer is per workspace.

```bash
trebi conversation list --json --limit 20
trebi conversation show <id> --json
trebi conversation messages <id> --json --limit 50
trebi conversation new --title "CI firefight"
trebi conversation switch <id>
```

`conversation switch` is **local only** (sticky CLI pointer); remote: pass `room` on `assistant.say`, or `pointer_set`.

## Assistant memory

The assistant keeps a structured `MEMORY.md` at the workspace root. It holds four sections in a fixed order: Context, State, Lessons, Log. The assistant reads the memory at every process start.

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
