# machine

Machine operations: list the machines and read what one machine can run. A machine runs Trebi, and each workspace is on one machine. This machine is the self row (`is_self: true`). With a Trebi Cloud link, the self row has `device_id` and the machine name from the link.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

A state is `ready`, `missing`, `not_installed`, `off`, `logged_out`, `error`, or `unknown`. The reply has names and states only, never a secret value. To see what one job or agent needs, read `job.get` or `agent.get`: the `readiness` list has one item per harness, connector, and secret, each with `kind`, `name`, `state`, and `action` (the user verb that fixes it, for example `Log in`, `Install`, `Connect`, or `Add secret`).

## Actions

### list
List the machines, this machine first. The reply is `{"machines": [{"id", "name", "os", "arch", "device_id", "is_self", "last_seen", "visor_build", "revision", "created_at", "updated_at", "link"}]}`. `link` is null on this machine. On another machine it is `{"state", "error", "linked_at"}`: `state` is `none` (not connected), `linked`, or `error` (the last connection failed; `error` says why).

### get
Read one machine. For this machine the reply also has `capabilities`: `{"harnesses": [{"name", "label", "installed", "enabled", "version", "state"}], "connectors": [{"id", "name", "type", "title", "version", "state"}], "versions": {"trebi", "edition", "ui"}}`.
- `id` (string, required): the machine id, or `self` for this machine.

### connect
Connect this machine to another machine of the same Trebi Cloud account. Then the user can open that machine from the Trebi app on this machine. On the other machine, the user makes a pairing link first (`mobile.pair` there, or "Let another machine connect" in Machines). The link works once. The reply is the machine, with `link.state` `linked`. A link for another machine, or a link that is not a pairing link, replies `400 validation`. A used or expired link replies `409 conflict`. When the other machine does not answer, the reply is `502 unavailable`.
- `id` (string, required): the id of the other machine, from `list`.
- `link` (string, required): the pairing link that the other machine made.

### disconnect
Remove the link to another machine, here and in the cloud. To connect again, the other machine must make a new pairing link. Ask the user first, then repeat the call with `confirm: true`.
- `id` (string, required): the id of the other machine.
- `confirm` (boolean, required): true once the user agrees.

### sessions
List the agent sessions that the user started on this machine outside Trebi, the last active first. Trebi sees them only when the hooks of the harness are on (`harness.set_hooks`). Trebi does not start or stop them. The reply is `{"sessions": [{"id", "harness", "folder", "state", "turns", "waiting", "last_activity", "started_at"}]}`. `state` is `starting`, `running`, `waiting` (the session waits for the user), `idle`, `lost`, or `exited`. Another machine replies `409 remote_machine`.
- `id` (string, required): the machine id, or `self` for this machine.

### session
Read one session that started outside Trebi, with its transcript. The reply is `{"session": {…as in sessions}, "turns": [{"at", "role", "text"}]}`. `role` is `user`, `assistant`, `request` (the session asks the user), or `error`.
- `id` (string, required): the machine id, or `self` for this machine.
- `session` (string, required): the session id from `sessions`.

## Related guides

`mcp-basics`
