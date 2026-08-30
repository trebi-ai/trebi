# workspace

Workspace operations: register and manage workspaces.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List workspaces. No input fields.

### show
Read one workspace.
- `id` (string, required): workspace id.

### add
Register one directory as a workspace.
- `path` (string, required): absolute directory path.
- `name` (string, optional): display name.
- `create` (boolean, optional): create the directory when absent.

### patch
Change one workspace.
- `id` (string, required): workspace id.
- `name` (string, optional): new display name.
- `git_enabled` (boolean, optional): enable or disable git tracking.

### remove
Unregister one workspace. Destructive. Files on disk stay.
- `id` (string, required): workspace id.

## Related guides

`getting-started`
