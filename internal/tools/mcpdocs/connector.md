# connector

Connector operations: manage event connectors and their cursors.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List connectors. Includes managed rows from `connectors.yaml` and read-only
**inherited** MCP servers discovered from user harness config. Inherited rows
carry `provenance: inherited`, `managed: false`, `origins[]` (tool, path, key),
and `injected` (whether a trebi session of that harness will load them).
- `limit` (number, optional): page size.

### show
Read one connector by name or `id`. Duplicate names return `ambiguous`; use `id`.
- `name` (string, required): connector name or id.

### create
Create one connector from a definition object.
- `definition` (object, required): connector fields, for example `{"name": "mail", "command": "fetch-mail.sh"}`.

Mutations (`create`, `set`, `delete`, `connect`, `disconnect`, `restart`, `test`,
`cursor_set`) on an inherited row return `conflict`.

### set
Create or update one connector from flat fields.
- `name` (string, required): connector name.
- `command` (string, optional): command that emits events.
- `format` (string, optional): event format.
- `type` (string, optional): connector type.
- `cwd` (string, optional): working directory.
- `env` (object, optional): string-to-string environment map.
- `facets` (array, optional): facet names this connector serves.

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
