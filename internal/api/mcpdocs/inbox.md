# inbox

Inbox operations: read and resolve notifications, and read alerts.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List inbox items.
- `status` (string, optional): filter by status.
- `kind` (string, optional): filter by kind.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### show
Read one inbox item.
- `id` (string, required): item id.

### read
Mark one inbox item as read.
- `id` (string, required): item id.

### read_all
Mark every unread inbox item as read. No input fields.

### resolve
Resolve one inbox item.
- `id` (string, required): item id.

### alerts
List alerts raised since a time.
- `since` (string, optional): RFC3339 cut-off; omit for all.

## Related guides

`inbox-and-alerts`
