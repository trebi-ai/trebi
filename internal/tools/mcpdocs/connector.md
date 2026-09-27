# connector

Connector operations: manage event connectors and their cursors, and search or
install MCP registry servers.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List connectors. Includes managed rows from `connectors.yaml` and read-only
**inherited** MCP servers discovered from user harness config. Inherited rows
carry `provenance: inherited`, `managed: false`, `origins[]` (tool, path, key),
and `injected` (whether a trebi session of that harness will load them).
A running row carries `status`, `pid`, `restarts`, and `last_error` from its
supervisor. A row whose definition has no `events` block has no supervisor and
no status.
- `limit` (number, optional): page size.
- `capability` (string, optional): keep rows with `actions`, `events`, or `channel`.
- `provenance` (string, optional): keep rows with `native`, `managed`, or `inherited`.

### show
Read one connector by name or `id`. Duplicate names return `ambiguous`; use `id`.
- `name` (string, required): connector name or id.

### search
Search the configured MCP registries for installable servers. With no
`registry` the action reads every configured registry in order and merges the
pages. The reply carries `entries` and, when more exist, `next_cursor`.
- `q` (string, optional): substring match on the server name.
- `registry` (string, optional): one configured registry name; empty reads all.
- `limit` (number, optional): page size per registry.
- `cursor` (string, optional): opaque cursor from a prior page (`next_cursor`).

### catalog_entry
Read one server entry and its declared environment variables. Each variable
carries `required` and `secret` flags, so a caller knows what to pass to
`install`.
- `name` (string, required): server name, for example `ai.adtest/adtest-mcp`.
- `registry` (string, optional): registry name.
- `version` (string, optional): server version; empty asks for the latest.

### install
Install one registry server as a connector with an `actions.mcp` block and a
`source` block. **This runs third-party code on this machine** (through npx,
uvx, or docker) when a run uses the connector. A variable the entry marks
`isSecret` is written to the secret store and referenced as
`secret:<connector>_<VAR>`. A required variable with no value returns
`validation` and names the missing variables. A connector name that already
exists returns `conflict`.
- `name` (string, required): server name.
- `registry` (string, optional): registry name.
- `version` (string, optional): server version; empty uses the latest.
- `as` (string, optional): connector name; empty derives one from the server name.
- `package` (number, optional): package index to install; this wins over `remote`.
- `remote` (number, optional): remote index to install when no package is chosen.
- `env` (object, optional): variable values as `{VAR: value}`; a value for a secret variable becomes a secret.

### create
Create one connector from a definition object. Each block is optional; a
connector with none is a bare definition.
- `definition` (object, required): a definition such as
  `{"name": "mail", "events": {"command": "fetch-mail.sh", "format": "line"}}`.
  Blocks: `events` (`command` required; `format`, `restart`, `poll_ms`, `id`,
  `ts`, `identity`, `catchup{command,max_events,timeout_ms}`), `actions`
  (`mcp{command,args,env,url,headers}` and/or `cli{commands,skill}`), `channel`
  (`ref`, `send` required; `typing`, `typing_interval_ms`, `typing_max_ms`, `ack`),
  plus `env` and `cwd`.

Mutations (`create`, `replace`, `set`, `delete`, `connect`, `disconnect`,
`restart`, `test`, `cursor_set`, `install`) on an inherited row return `conflict`.

### replace
Replace the whole definition of one connector (`PUT`). Use it for an import:
the body is the new definition, and anything absent is gone.
- `name` (string, required): connector name.
- `definition` (object, required): the complete definition, as in `create`
  without `name`.

### set
Update one connector (`PATCH`). Each key given replaces that part of the
definition; keys left out stay. `env` merges into the stored map. Send `null`
for `actions`, `events`, or `channel` to remove that block. A rename is not
possible; use `create` and `delete`.
- `name` (string, required): connector name.
- `cwd` (string, optional): working directory.
- `env` (object, optional): string-to-string environment map; merges.
- `events` (object, optional): whole events block, same shape as in `create`
  (`{"command": "fetch-mail.sh", "poll_ms": 900000}`), or `null` to remove it.
- `actions` (object, optional): whole actions block
  (`{"mcp": {"command": "npx", "args": ["-y", "server"]}}`), or `null`.
- `channel` (object, optional): whole channel block
  (`{"ref": "chat-1", "send": "..." }`), or `null`.
A connector with an `events` block runs a supervisor; changing `events` while it
runs restarts it, and changing only `actions` does not.

### delete
Remove one connector and stop its event stream. Destructive.
- `name` (string, required): connector name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

### connect
Start one connector process.
- `name` (string, required): connector name.

### disconnect
Stop one connector process.
- `name` (string, required): connector name.

### restart
Restart one connector process.
- `name` (string, required): connector name.

### test
Send one test event through a connector.
- `name` (string, required): connector name.
- `event` (any, optional): event payload.

### logs
Read the log tail of one connector. Follow mode is not available; poll instead.
The response is `{"lines": [...]}`, newest last, with `truncated: true` when `lines` cut it.
- `name` (string, required): connector name.
- `lines` (number, optional): keep only the last N lines.
- `max_bytes` (number, optional): cap the body at this size; default 64 KB. A cut body returns `{"content": "…", "truncated": true}`.

### cursor_show
Read the event cursor of one connector.
- `name` (string, required): connector name.

### cursor_set
Set the event cursor of one connector.
- `name` (string, required): connector name.
- `last_event_id` (string, required): cursor value.

### cursor_reset
Reset the event cursor of one connector to the start.
- `name` (string, required): connector name.

## Related guides

`connectors-and-rules`
