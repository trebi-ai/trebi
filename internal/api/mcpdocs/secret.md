# secret

Secret operations: manage named secrets used by jobs and connectors.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

Caution: a secret value you send passes through the model context of this session. Prefer a secret that is already scoped to one purpose.

## Actions

### list
List secret names. Values are never returned. No input fields.
- `limit` (number, optional): maximum rows to return.

### set
Set one secret value.
- `name` (string, required): secret name.
- `value` (string, required): secret text. Prefer `value_file`, so the value stays out of your transcript.
- `value_file` (string, optional): path to a local file holding the value. `trebi mcp serve` reads it and sends the content as `value`. Only a stdio bridge can do this; over plain HTTP, pass `value`.

### delete
Remove one secret. Destructive. Jobs that read it will fail.
- `name` (string, required): secret name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

## Related guides

`skills-secrets-config`
