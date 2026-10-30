# machine

Machine operations: list the machines and read what one machine can run. A machine runs Trebi, and each workspace is on one machine. This machine is the self row (`is_self: true`). With a Trebi Cloud link, the self row has `device_id` and the machine name from the link.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

A state is `ready`, `missing`, `not_installed`, `off`, `logged_out`, `error`, or `unknown`. The reply has names and states only, never a secret value. To see what one job or agent needs, read `job.get` or `agent.get`: the `readiness` list has one item per harness, connector, and secret, each with `kind`, `name`, `state`, and `action` (the user verb that fixes it, for example `Log in`, `Install`, `Connect`, or `Add secret`).

## Actions

### list
List the machines, this machine first. The reply is `{"machines": [{"id", "name", "os", "arch", "device_id", "is_self", "last_seen", "visor_build", "revision", "created_at", "updated_at"}]}`.

### get
Read one machine. For this machine the reply also has `capabilities`: `{"harnesses": [{"name", "label", "installed", "enabled", "version", "state"}], "connectors": [{"id", "name", "type", "title", "version", "state"}], "versions": {"trebi", "edition", "ui"}}`.
- `id` (string, required): the machine id, or `self` for this machine.

## Related guides

`mcp-basics`
