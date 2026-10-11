---
topic: mcp-basics
title: MCP entry point
description: Tool/action/input shape, docs actions, CLI-to-MCP translation, workspace, errors, pagination, and what MCP cannot do.
---

# MCP entry point

Trebi's control plane is also an MCP server at `/mcp` (Streamable HTTP, no auth). Twenty-five tools, one per object family (`run`, `job`, `workspace`, `artifact`, `decision`, `email`, `inbox`, and more; `tools/list` gives all). Each tool takes `{"action": "<name>", "input": {…}}`. Action `docs` returns the input field reference for that tool. Errors return the HTTP error envelope unchanged.

Connect with the URL from `~/.trebi/api.json` (`{"url"}`). The daemon serves its whole API without a credential; the bind address is the only gate.

`trebi mcp` writes that config for you. It is **local only** and works with the daemon down:

```bash
trebi mcp config --client claude          # the config block for one client
trebi mcp config --client claude --command  # the add command instead
trebi mcp install --client claude         # runs the add command
trebi mcp serve                           # stdio bridge for a client without HTTP
```

Every `mcp` command is **local only**, except `trebi mcp config --remote`. Clients: `claude`, `codex`, `opencode`, `cursor`, `vscode`, and `json` for the raw url. `install` covers `claude`, `codex`, and `opencode`.

## Remote access (Pro Cloud)

Claude (web, Desktop, mobile) and ChatGPT cannot reach a local URL. A machine linked to Trebi Cloud with the `remote_access` feature can serve them through the cloud. It is on by default. Turn it off with `trebi config set cloud.remote_mcp false`. `trebi mcp config --remote` prints the URL (`https://staging-mcp.trebi.ai` on staging) and the steps for each client. The client signs in to the Trebi account with OAuth, and the user selects the machine and the access level (Read only or Full control). A remote caller can never shut down the daemon, link or unlink the cloud, or change `cloud.remote_mcp`. The cloud forwards each request and does not store or log its body.

## Translation rule

Guides show CLI examples: `trebi <tool> <action> --flag value <name>`.
As an MCP client, call tool `<tool>` with
`{"action": "<action>", "input": {"flag": "value", "name": "<name>"}}`.
The tool's `docs` action lists the exact input fields.

Hyphens in flags become underscores in input keys (`--if-match` → `if_match`, `--extra-prompt` → `extra_prompt`). The positional argument is the primary identifier (`name` / `id` / `topic`). Nested CLI words join with an underscore (`job memory lesson add` → `job.memory_lesson_add`, `connector cursor show` → `connector.cursor_show`).

CLI area names that differ from the tool name:

| CLI | MCP tool |
|---|---|
| `runs` | `run` |
| `doctor` | `system.doctor` |
| `bus` | `system.bus_poll` |
| `alerts` | `inbox.alerts` |
| `rules test` / `rules fire` | `event.rules_test` / `event.rules_fire` |
| `job file` / `skill file` / `file` | `file` (`owner`: `job` / `skill` / `workspace`) |
| `queue` (no subcommand) | `queue.list` |
| `service logs` / `drain` / `undrain` / `shutdown` | `system.logs` / `drain` / `undrain` / `shutdown` |
| `service status` | `system.health` |
| `service version` | `system.version` |
| `cloud status` / `link` / `unlink` / `addresses` | `system.cloud_status` / `cloud_link` / `cloud_unlink` / `cloud_addresses` |
| `hooks create` / `list` / `delete` | `system.hook_create` / `hook_list` / `hook_delete` |
| `harness auth status` / `login` / `logout` / `mode` / `cancel` | `harness.auth` / `login_start` / `logout` / `patch` / `login_cancel` |
| `job/agent/assistant memory log add` / `log delete` | same tool: `memory_add` / `memory_delete` (on `assistant`, `job`, or `agent`; `system` has no memory actions) |

These CLI commands have no MCP action; they are **local only**: `job status`,
`job tail`, `job schedule`, `conversation switch`, the offline
`memory validate` / `memory skeleton`, and `memory migrate` (the daemon
migrates legacy files at launch).

`--json` in examples is a no-op remotely; responses are already the API payload. Local path flags (`--file`, `--from`, `--from-file`, `--out`, `--event-file`, `--token-stdin`) become the document/value fields (`content`, `value`, `event`, `token`).

Call `docs` on a tool before first use. Call `system.guide` with topic `mcp-basics` before your first workflow, and `system.guides` for the index.

## No sticky context

`trebi workspace use` does not exist remotely; pass `workspace` in `input` on every workspace-scoped call. `workspace current` is **local only**.

## No daemon lifecycle

If the daemon is down there is no MCP server to talk to; `service start` / `install` / `stop` / `daemon` guidance does not apply. Health checks are `system` actions `health` and `doctor`.

## No interactive surfaces

`runs attach` and `assistant chat` are **local only**; the remote alternatives are `run.transcript` / `run.logs` polling and `assistant.say` + `conversation.messages`. `harness auth login --attach` is **local only**; remote: `login_start` / `login_token` / `login_code`.

## Verbs

Actions use one verb set: `list`, `show`, `create`, `set` (replace a whole
document), `patch` (change named fields), `delete`, plus `clear` for a document
you empty and `cancel` for in-flight work. The CLI keeps the older spellings
(`add`, `new`, `remove`, `update`) as aliases; guides and docs use the
canonical one.

## Destructive actions

A destructive action refuses the first call: it answers `validation` with
`details.confirm_required` and a message that names what it deletes. Ask the
user, then repeat the call with `confirm: true`.

## Errors

Every error is `{"error":{"code","message","details"}}` with `isError: true`. Stable codes: `not_found`, `conflict`, `ambiguous`, `validation`, `draining`, `precondition_failed`, `unauthorized`, `internal`. List first on `not_found`. Use the id on `ambiguous`. Re-read then write on `precondition_failed`.

## Pagination

List actions take `limit` and `cursor`. The cursor is the id of the last row. `system.bus_poll` returns `{"events":[…],"next_cursor":N}` — pass `cursor` back; there is no follow mode.

## What MCP cannot do

No WebSocket attach, no streaming bus follow, no `workspace use` sticky context, no OS service install/start/stop, no `trebi guide` (use `system.guide` / `system.guides`), no local path flags. Prefer the CLI when both paths exist on the same machine.
