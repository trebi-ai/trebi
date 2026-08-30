# secret

Secret operations: manage named secrets used by jobs and integrations.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

Caution: a secret value you send passes through the model context of this session. Prefer a secret that is already scoped to one purpose.

## Actions

### list
List secret names. Values are never returned. No input fields.

### set
Set one secret value.
- `name` (string, required): secret name.
- `value` (string, required): secret text.

### delete
Remove one secret. Destructive.
- `name` (string, required): secret name.
