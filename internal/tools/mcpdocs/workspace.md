# workspace

Workspace operations: register and manage workspaces.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

There is no sticky workspace over MCP: `trebi workspace use` writes client-side CLI state, not daemon state. Pass `workspace` on each call that takes it, using the id from `list`.

## Actions

### list
List workspaces. Each row carries `job_count` and `running_count`.
- `limit` (number, optional): maximum rows to return.

### show
Read one workspace. The row carries the resolved `email_allowlist` of the workspace, so it shows what `patch` wrote.
- `id` (string, required): workspace id.

### create
Register one directory as a workspace.
- `path` (string, required unless you mint one): absolute directory path.
- `name` (string, optional): display name. Required for a pathless add.
- `handle` (string, optional): the fixed name in the workspace email address, `<handle>-<token>@<domain>`. 2 to 20 characters: a-z, 0-9, and inner dashes. When absent, the daemon derives it from the name. It cannot change later, except with `email.workspace_rotate`. Ask the user for it when they care how the address reads.
- `mkdir` (boolean, optional): create the directory when absent.

Pass `create: true` with `name` and no `path` to mint a folder under the trebi home.

### patch
Change one workspace.
- `id` (string, required): workspace id.
- `name` (string, optional): new display name.
- `git_enabled` (boolean, optional): enable or disable git tracking.
- `contact` (string or list, optional): the workspace contact addresses; `null` inherits.
- `notify` (list, optional): the workspace alert route; `null` inherits.
- `chains` (object, optional): the agent chain settings in `trebi.yaml` `agents.chains`. Give only the keys to change: `enabled` (agents can ask each other; default true), `max_hops` (depth limit, 1 to 20; default 5), `max_turns` (turn limit, 1 to 500; default 20), `max_usd` (cost limit; 0, the default, is no limit), `pair_per_hour` (asks from one agent to another in one hour, 1 to 1000; default 10). A value out of range returns `validation`. The workspace row carries the resolved `chains` block.
- `email_allowlist` (list or null, optional): the addresses that may receive mail from the workspace and send mail to it, in `trebi.yaml` `email.allowlist`. Each entry is one exact address; Trebi lowercases it and trims it. A domain entry, or an entry without `@`, returns `validation` with `field: email_allowlist`. `null` clears the list. The workspace row carries the resolved `email_allowlist`.

### move
Move the workspace folder to a new absolute path and rewrite absolute path prefixes in its files (jobs, scripts). Connector `cwd` / command / env values that point at the old path are rewritten too. Historical run rows are left alone.
- `id` (string, required): workspace id.
- `path` (string, required): new absolute directory path.
- `mkdir` (boolean, optional): create the destination parent when absent.
- `dry_run` (boolean, optional): report rewrites without moving.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`. Live runs in the workspace return `conflict`.

If the folder was already moved on disk and `trebi.yaml` still has this id, the call rebinds the registry to `path` and rewrites files there.

### delete
Unregister one workspace and delete its folder on disk. Destructive. Run rows stay.
- `id` (string, required): workspace id.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required` and a message that names what goes.

## Related guides

`getting-started`
