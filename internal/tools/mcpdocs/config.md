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

### unset
Clear a config key. For `assistant.api_key`, this also deletes the stored secret so env fallback can take over.
- `key` (string, required): config key, for example `assistant.api_key`.

### validate
Check the config. The reply is `{"ok": true}`, or `{"ok": false, "error": "…"}`
with the problems named. No input fields.

### validate_draft
Check a change against a copy of the config. Nothing is written. The reply is `{"ok": bool, "errors": [{"key", "message"}]}`. Give at least one field.
- `set` (object, optional): dotted keys and new values. A list or an object, for example for `notify.route`, is sent as JSON text.
- `unset` (array, optional): keys to clear, as strings.
- `yaml` (string, optional): full YAML text that replaces `config.yaml` in the draft.

### schema
List every key with `section`, `type` (`string|int|bool|duration|list|map|json`), `enum`, `default`, `description`, `secret`, and `restart`. A `restart` key takes effect after a daemon restart. No input fields.

### yaml
Read `config.yaml` as text, with secret values masked as `***`. The reply is `{"yaml", "hash"}`. A `set` with `***` keeps the stored value. No input fields.

## Related guides

`skills-secrets-config`
