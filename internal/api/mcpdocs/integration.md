# integration

Integration operations: manage event integrations and their cursors.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List integrations.
- `limit` (number, optional): page size.

### show
Read one integration.
- `name` (string, required): integration name.

### create
Create one integration from a definition object.
- `definition` (object, required): integration fields, for example `{"name": "mail", "command": "fetch-mail.sh"}`.

### set
Create or update one integration from flat fields.
- `name` (string, required): integration name.
- `command` (string, optional): command that emits events.
- `format` (string, optional): event format.
- `type` (string, optional): integration type.
- `cwd` (string, optional): working directory.
- `env` (object, optional): string-to-string environment map.

### remove
Remove one integration. Destructive.
- `name` (string, required): integration name.

### connect
Start one integration process.
- `name` (string, required): integration name.

### disconnect
Stop one integration process.
- `name` (string, required): integration name.

### restart
Restart one integration process.
- `name` (string, required): integration name.

### test
Send one test event through an integration.
- `name` (string, required): integration name.
- `event` (any, optional): event payload.

### logs
Read the log tail of one integration. Follow mode is not available; poll instead.
- `name` (string, required): integration name.

### cursor_show
Read the event cursor of one integration.
- `name` (string, required): integration name.

### cursor_set
Set the event cursor of one integration.
- `name` (string, required): integration name.
- `last_event_id` (string, required): cursor value.

### cursor_reset
Reset the event cursor of one integration to the start.
- `name` (string, required): integration name.
