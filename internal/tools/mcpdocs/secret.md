# secret

Secret operations: manage named secrets used by jobs and connectors.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

Caution: a secret value you send passes through the model context of this session. Prefer a secret that is already scoped to one purpose.

## Actions

### list
List secret names. Values are never returned. `owners` maps each name that Trebi writes (an AI app login, the Trebi account key, a phone key, a machine link, the assistant key) to `{kind, label, link}`; `set` and `delete` refuse those names with `conflict`.
- `limit` (number, optional): maximum rows to return.

### set
Set one secret value.
- `name` (string, required): secret name.
- `value` (string, required): secret text. Prefer `value_file`, so the value stays out of your transcript.
- `value_file` (string, optional): path to a file holding the value. Resolved before dispatch (native registry and `trebi mcp serve`) so the path never reaches the API. Prefer this over `value`.

### delete
Remove one secret. Destructive. Jobs that read it will fail.
- `name` (string, required): secret name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

## Related guides

`skills-secrets-config`
