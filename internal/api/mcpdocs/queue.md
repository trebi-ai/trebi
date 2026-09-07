# queue

Queue operations: inspect and steer the run queue.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List queued runs.
- `status` (string, optional): filter by queue status.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### requeue
Move one queued run back to the ready state.
- `id` (string, required): queue entry id.

### cancel
Drop one queued run, or several.
- `id` (string, required unless `ids` is given): queue entry id.
- `ids` (array, optional): cancel several entries in one call. The reply carries `ok`, `count`, and `failed`.

## Related guides

`connectors-and-rules`
