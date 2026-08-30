# config

Config operations: read and change daemon settings.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### show
Read the effective config. No input fields.

### set
Change one config key.
- `key` (string, required): config key, for example `daemon.log_level`.
- `value` (string, required): new value text.
