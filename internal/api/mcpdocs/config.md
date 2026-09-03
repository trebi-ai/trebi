# config

Config operations: read and change daemon settings.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### show
Read the effective config. No input fields.

### set
Change config keys. Give `values`, or `key` with `value`.
- `values` (object, optional): several dotted keys at once, for example `{"daemon.log_level": "debug", "limits.max_file_mb": 20}`.
- `key` (string, required unless `values` is given): config key, for example `daemon.log_level`.
- `value` (any, optional, required with `key`): new value; a string, a number, or a boolean.

### validate
Check the config. The reply is `{"ok": true}`, or `{"ok": false, "error": "…"}`
with the problems named. No input fields.

## Related guides

`skills-secrets-config`
