# outbox

Outbox operations: read, retry, and cancel outgoing messages. One outbox row is one channel reply, one email, or one alert notification.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

A row goes `pending` → `sending` → `sent`. A failed attempt sets `error` and `next_attempt_at` and keeps the row `pending`. After `max_attempts` the row becomes `failed`. The daemon sends each row at least once: after a crash, a row can go out two times.

## Actions

### list
List outbox rows, newest first. The reply is `{"outbox": [...], "next_cursor"}`.
- `kind` (string, optional): `email` or `channel`.
- `status` (string, optional): `pending`, `sending`, `sent`, `failed`, or `canceled`.
- `purpose` (string, optional): `reply`, `send`, `alert`, `report`, or `test`.
- `workspace` (string, optional): workspace name or id.
- `conversation` (string, optional): conversation id.
- `inbox` (string, optional): inbox item id.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### get
Read one outbox row, with its body and its last error.
- `id` (string, required): row id.

### retry
Send a `failed` or `canceled` row again. Attempts go back to zero. Any other status returns 409 `conflict`.
- `id` (string, required): row id.

### cancel
Stop a `pending` or `failed` row. Any other status returns 409 `conflict`.
- `id` (string, required): row id.

## Related guides

`inbox-and-alerts`
