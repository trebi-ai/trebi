---
topic: mcp-basics
title: MCP entry point
description: Tool/action/input shape, docs actions, CLI-to-MCP translation, workspace, errors, pagination, and what MCP cannot do.
---

# MCP entry point

Trebi's control plane is also an MCP server at `/mcp` (Streamable HTTP, bearer auth). Fifteen tools (`run`, `job`, `workspace`, `assistant`, `file`, `harness`, `integration`, `events`, `queue`, `inbox`, `conversation`, `skill`, `secret`, `config`, `system`). Each tool takes `{"action": "<name>", "input": {…}}`. Action `docs` returns the input field reference for that tool. Errors return the HTTP error envelope unchanged.

Connect with the URL and token from `~/.trebi/api.json` (`{"url", "token"}`).

## Translation rule

Guides show CLI examples: `trebi <tool> <action> --flag value <name>`.
As an MCP client, call tool `<tool>` with
`{"action": "<action>", "input": {"flag": "value", "name": "<name>"}}`.
The tool's `docs` action lists the exact input fields.

Hyphens in flags become underscores in input keys (`--if-match` → `if_match`, `--extra-prompt` → `extra_prompt`). The positional argument is the primary identifier (`name` / `id` / `topic`). Nested CLI words join with an underscore (`job memory show` → `job.memory_show`, `integration cursor show` → `integration.cursor_show`).

CLI area names that differ from the tool name:

| CLI | MCP tool |
|---|---|
| `runs` | `run` |
| `doctor` | `system.doctor` |
| `bus` | `system.bus_poll` |
| `alerts` | `inbox.alerts` |
| `rules test` / `rules fire` | `events.rules_test` / `events.rules_fire` |
| `job file` / `skill file` / `file` | `file` (`owner`: `job` / `skill` / `workspace`) |
| `queue` (no subcommand) | `queue.list` |
| `service logs` / `drain` / `undrain` / `shutdown` | `system.logs` / `drain` / `undrain` / `shutdown` |
| `service status` | `system.health` |

`--json` in examples is a no-op remotely; responses are already the API payload. Local path flags (`--file`, `--from`, `--from-file`, `--out`, `--event-file`, `--token-stdin`) become the document/value fields (`content`, `value`, `event`, `token`).

Call `docs` on a tool before first use. Call `system.guide` with topic `mcp-basics` before your first workflow, and `system.guides` for the index.

## No sticky context

`trebi workspace use` does not exist remotely; pass `workspace` in `input` on every workspace-scoped call. `workspace current` is **local only**.

## No daemon lifecycle

If the daemon is down there is no MCP server to talk to; `service start` / `install` / `stop` / `daemon` guidance does not apply. Health checks are `system` actions `health` and `doctor`.

## No interactive surfaces

`runs attach` and `assistant chat` are **local only**; the remote alternatives are `run.transcript` / `run.log` polling and `assistant.say` + `conversation.messages`. `harness auth login --attach` is **local only**; remote: `login_start` / `login_token` / `login_code`.

## Errors

Every error is `{"error":{"code","message","details"}}` with `isError: true`. Stable codes: `not_found`, `conflict`, `ambiguous`, `validation`, `draining`, `precondition_failed`, `unauthorized`, `internal`. List first on `not_found`. Use the id on `ambiguous`. Re-read then write on `precondition_failed`.

## Pagination

List actions take `limit` and `cursor`. The cursor is the id of the last row. `system.bus_poll` returns `{"events":[…],"next_cursor":N}` — pass `cursor` back; there is no follow mode.

## What MCP cannot do

No WebSocket attach, no streaming bus follow, no `workspace use` sticky context, no OS service install/start/stop, no `trebi guide` (use `system.guide` / `system.guides`), no local path flags. Prefer the CLI when both paths exist on the same machine.
