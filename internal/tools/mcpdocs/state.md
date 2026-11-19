# state

State operations: read and write the records of a job or an agent. A record is a row with a key, an optional time `at`, and a JSON object `value`. Rows live in named collections. Keep records here, not in MEMORY.md.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

The `owner` field selects whose state you use:
- `self` (the default): the job or agent of this run. Outside a run, `self` returns `validation`.
- A job or agent ID (`jb_…`, `agt_…`).
- A job or agent name; then set `workspace`.

A run can only use the state of its own job or agent. An agent run can also use the state of the jobs that it owns. Another owner returns `state_scope`. A run deletes its own rows with no confirm.

Times (`since`, `until`, row `at`) take `2026-09-01`, `2026-09-01 10:00`, RFC 3339, `12h`, `7d`, `2w`, `3mo`, `1y`, `today`, `yesterday`, or `last-run`. A relative value counts back from now.

## Actions

### summary
Show each collection with its row count, size, newest time, fields, and the change count of the last run.
- `owner` (string, optional): `self` (default), an ID, or a name.
- `workspace` (string, optional): workspace id or name; needed when `owner` is a name.

### query
Find rows. The order is: key, prefix, time, and text filters; then `where`; then `sort`; then `limit` and `fields`.
- `collection` (string, required): collection name.
- `owner` (string, optional): `self` (default), an ID, or a name.
- `workspace` (string, optional): workspace id or name; needed when `owner` is a name.
- `keys` (array, optional): exact keys.
- `prefix` (string, optional): key prefix.
- `q` (string, optional): full-text match on the key and the string fields.
- `where` (string, optional): JavaScript on `v` (value), `k` (key), and `at` that returns a boolean, for example `v.score >= 2`.
- `since` (string, optional): start time, inclusive.
- `until` (string, optional): end time, exclusive.
- `by` (string, optional): the time that `since` and `until` use: `at` (default), `updated`, or `created`.
- `sort` (string, optional): `at`, `-at`, `updated`, `created`, `key`, or a value path such as `-v.score`.
- `fields` (array, optional): fields to return, for example `["key", "at", "title"]`. Default: key, at, and each top-level value field.
- `limit` (number, optional): rows per page; default 50, max 500.
- `cursor` (string, optional): the `next` value of the prior page.

### get
Read full rows by key. A missing key has no row.
- `collection` (string, required): collection name.
- `keys` (array, required): keys to read.
- `owner` (string, optional): `self` (default), an ID, or a name.
- `workspace` (string, optional): workspace id or name; needed when `owner` is a name.

### has
Check keys. Returns `found` (key to time) and `missing`.
- `collection` (string, required): collection name.
- `keys` (array, required): keys to check.
- `owner` (string, optional): `self` (default), an ID, or a name.
- `workspace` (string, optional): workspace id or name; needed when `owner` is a name.

### put
Write a batch of rows. All rows are written, or none. A bad batch returns `state_invalid` with `details.errors`, one entry for each failed check: `{line, path, msg}`.
- `collection` (string, required): collection name.
- `rows` (array, required): rows `{key, at, value, inc}`. `value` is a JSON object. `inc` adds numbers to value fields (merge mode only).
- `mode` (string, optional): `upsert` (default) replaces a row, `insert` fails on a key that exists, `merge` patches the value.
- `owner` (string, optional): `self` (default), an ID, or a name.
- `workspace` (string, optional): workspace id or name; needed when `owner` is a name.

### delete
Delete rows by key. Destructive.
- `collection` (string, required): collection name.
- `keys` (array, required): keys to delete.
- `confirm` (boolean, required outside a run): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `owner` (string, optional): `self` (default), an ID, or a name.
- `workspace` (string, optional): workspace id or name; needed when `owner` is a name.

### prune
Delete the rows in a time range. Destructive. Set `since` or `until`.
- `collection` (string, required): collection name.
- `since` (string, optional): start time, inclusive.
- `until` (string, optional): end time, exclusive, for example `180d`.
- `by` (string, optional): `at` (default), `updated`, or `created`.
- `confirm` (boolean, required outside a run): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `owner` (string, optional): `self` (default), an ID, or a name.
- `workspace` (string, optional): workspace id or name; needed when `owner` is a name.

### drop
Delete all rows of a collection. Destructive.
- `collection` (string, required): collection name.
- `confirm` (boolean, required outside a run): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `owner` (string, optional): `self` (default), an ID, or a name.
- `workspace` (string, optional): workspace id or name; needed when `owner` is a name.

## Related guides

`jobs-and-scheduling`, `agents`
