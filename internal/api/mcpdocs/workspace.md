# workspace

Workspace operations: register and manage workspaces.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

There is no sticky workspace over MCP: `trebi workspace use` writes client-side CLI state, not daemon state. Pass `workspace` on each call that takes it, using the id from `list`.

## Actions

### list
List workspaces. No input fields.

### show
Read one workspace.
- `id` (string, required): workspace id.

### add
Register one directory as a workspace.
- `path` (string, required unless you mint one): absolute directory path.
- `name` (string, optional): display name. Required for a pathless add.
- `create` (boolean, optional): create the directory when absent.

Pass `create: true` with `name` and no `path` to mint a folder under the trebi home.

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
