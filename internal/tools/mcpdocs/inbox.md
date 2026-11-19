# inbox

Inbox operations: list items, mark them read or unread, archive them to Done, and read alerts.

An item is open (in the inbox) or Done. The daemon closes an item when its cause goes away (`done_by: "system"`). The user archives an item to Done (`done_by: "user"`). Read state is separate from Done state. `needs_action` is true for an open approval, an open request, and an open blocker or critical alert. The daemon deletes a Done item after `inbox.done_retention` (default 720h).

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List inbox items, newest first. The reply is `{items, next_cursor, counts}`. `counts` has `inbox` (open items), `done`, and `unread` (open unread items per kind).
- `view` (string, optional): `inbox` (open items, default) or `done`.
- `kind` (string, optional): `approval`, `request`, `failure`, `health`, `watchdog`, or `escalation`.
- `alert_kind` (string, optional): only items of this alert kind, for example `route_wait` (a message that waits for you to choose a route).
- `connector` (string, optional): only items of this connection slug.
- `workspace` (string, optional): workspace id or name; global items always match.
- `unread` (boolean, optional): only unread items.
- `query` (string, optional): text to find in the title, subject, or body.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size (max 200).

### show
Read one inbox item.
- `id` (string, required): item id.

### apply
Change the read or Done state of items. Give `ids` or `filter`, not both. The reply is `{updated: [ids]}`; it lists only the items that changed.
- `op` (string, required): `read`, `unread`, `archive`, or `unarchive`.
- `ids` (array, optional): item ids, at most 500.
- `filter` (object, optional): every item that matches. Fields: `view`, `kind`, `workspace`, `unread`, `query`, as in `list`.
- `confirm` (boolean, optional): an `archive` that holds an open approval or request answers `validation` with `details.confirm_required`, `details.count`, and `details.pending`. The run still waits after an archive. Ask the user, then repeat with `confirm: true`.

### alerts
List alerts raised since a time.
- `since` (string, optional): RFC3339 cut-off; omit for all.
- `limit` (number, optional): maximum rows to return.

## Related guides

`inbox-and-alerts`
