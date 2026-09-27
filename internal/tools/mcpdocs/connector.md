# connector

Connector operations: manage event connectors and their cursors, and search or
install MCP registry servers.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List connectors. Includes managed rows from `connectors.yaml` and read-only
**inherited** MCP servers discovered from user harness config. Inherited rows
carry `provenance: inherited`, `managed: false`, `origins[]` (tool, path, key),
and `injected` (whether a trebi session of that harness will load them).
A running row carries `status`, `pid`, `restarts`, and `last_error` from its
supervisor. A row whose definition has no `events` block has no supervisor and
no status.
When `connectors.yaml` does not load, the reply carries `load_error` with the
parse error. The daemon then refuses every connector write with `conflict`
until the file loads again. Fix the file by hand.
A row shows each literal value of `env`, `actions.mcp.env`, and
`actions.mcp.headers` as `***`. A value that holds a `secret:NAME` reference
stays visible. A write that sends `***` back keeps the stored value.
- `limit` (number, optional): page size.
- `capability` (string, optional): keep rows with `actions`, `events`, or `channel`.
- `provenance` (string, optional): keep rows with `native`, `managed`, or `inherited`.

### show
Read one connector by name or `id`. Duplicate names return `ambiguous`; use `id`.
- `name` (string, required): connector name or id.

### search
Search the configured MCP registries for installable servers. With no
`registry` the action reads every configured registry in order and merges the
pages. The reply carries `entries`, `registries` (the configured list as
`[{name, url}]`) and, when more exist, `next_cursor`. A registry that fails
does not fail the search: the reply lists it in
`registry_errors: [{registry, error}]` and the other registries still answer.
The search fails only when every registry fails.
- `q` (string, optional): substring match on the server name.
- `registry` (string, optional): one configured registry name; empty reads all.
- `limit` (number, optional): page size per registry.
- `cursor` (string, optional): opaque cursor from a prior page (`next_cursor`).

### catalog_entry
Read one server entry and the inputs that `install` accepts.
`package_inputs[i]` and `remote_inputs[i]` list the inputs of package `i` and
remote `i` as `{name, kind, description, format, default, choices, required,
secret}`. `kind` is `env`, `header`, `argument`, or `variable`. The `name` is
the key to pass in the install `env`.
- `name` (string, required): server name, for example `ai.adtest/adtest-mcp`.
- `registry` (string, optional): registry name.
- `version` (string, optional): server version; empty asks for the latest.

### install
Install one registry server as a connector with an `actions.mcp` block and a
`source` block. **This runs third-party code on this machine** (through npx,
uvx, docker, or the entry `runtimeHint`) when a run uses the connector. A
package must use the `stdio` transport. A remote URL must be `https` and must
not name a loopback, link-local, or private address.

`env` fills the inputs that `catalog_entry` lists: env variables, headers,
arguments, and `{name}` template variables. A value `secret:NAME` is kept as a
reference and writes no secret. A value for a secret input becomes the secret
`<connector>_<VAR>` (for a header, the whole rendered header value) and the
definition refers to it as `secret:<connector>_<VAR>`. A key that no input uses
returns `validation`; for a package, a key that is a valid env name is added to
the env instead. A required input with no value returns `validation` and names
the missing inputs.

A secret that exists with a different value returns `conflict` unless
`overwrite_secrets` is true. `source.secrets` lists the secrets that the
install created. `delete` removes them when no other connector refers to them.
A connector name or a contributed skill that already exists returns
`conflict`. A failed install removes what it wrote.
- `name` (string, required): server name.
- `registry` (string, optional): registry name.
- `version` (string, optional): server version; empty uses the latest.
- `as` (string, optional): connector name; empty derives one from the server name.
- `package` (number, optional): package index to install.
- `remote` (number, optional): remote index to install. Give `package` or `remote`, not both (`validation`). With neither, the install uses the first package, else the first remote.
- `env` (object, optional): input values as `{NAME: value}`.
- `overwrite_secrets` (boolean, optional): replace a stored secret that has a different value.

### create
Create one connector from a definition object. Each block is optional; a
connector with none is a bare definition.
- `definition` (object, required): a definition such as
  `{"name": "mail", "events": {"command": "fetch-mail.sh", "format": "line"}}`.
  Blocks: `events` (`command` required; `format`, `restart`, `poll_ms`, `id`,
  `ts`, `identity`, `catchup{command,max_events,timeout_ms}`), `actions`
  (`mcp{command,args,env,url,headers}` and/or `cli{commands,skill}`), `channel`
  (`ref`, `send` required; `typing`, `typing_interval_ms`, `typing_max_ms`, `ack`),
  plus `env` and `cwd`. `cwd` applies to the events command and the channel
  commands. It does not apply to an MCP server.

Channel `send` gets the reply text on stdin and `TREBI_CHANNEL_REF`,
`TREBI_CONVERSATION_ID`, and `TREBI_MESSAGE_ID` in its env. The reply goes to
the event's channel ref, not to the extracted `conversation_key`. Delivery is
at least once: after a crash the daemon can run `send` again for the same
reply. `TREBI_MESSAGE_ID` is the outbox row id and stays the same on each
retry, so `send` can use it to drop a repeat.

Mutations (`create`, `replace`, `set`, `delete`, `connect`, `disconnect`,
`restart`, `test`, `cursor_set`, `install`) on an inherited row return `conflict`.

### replace
Replace the whole definition of one connector (`PUT`). Use it for an import:
the body is the new definition, and anything absent is gone.
- `name` (string, required): connector name.
- `definition` (object, required): the complete definition, as in `create`
  without `name`.

### set
Update one connector (`PATCH`). Each key given replaces that part of the
definition; keys left out stay. `env` replaces the stored map. Send `null`
for `actions`, `events`, or `channel` to remove that block. A rename is not
possible; use `create` and `delete`.
- `name` (string, required): connector name.
- `cwd` (string, optional): working directory for the events and channel commands, not for an MCP server. An empty string clears it.
- `env` (object, optional): string-to-string environment map; replaces the stored map. To keep a stored value, send it back as `***`, as `show` returns it.
- `events` (object, optional): whole events block, same shape as in `create`
  (`{"command": "fetch-mail.sh", "poll_ms": 900000}`), or `null` to remove it.
- `actions` (object, optional): whole actions block
  (`{"mcp": {"command": "npx", "args": ["-y", "server"]}}`), or `null`.
- `actions_merge` (object, optional): field edits to the actions block, applied
  after `actions`: `{"mcp": {...}|null, "cli": {...}|null}`. An absent key
  stays, `null` removes that sub-block, and each field given replaces the stored
  field. A new `command` clears `url` and `headers`. A new `url` clears
  `command`, `args`, and `env`. Use it to change one field, for example
  `{"mcp": {"args": ["-v"]}}`.
- `channel` (object, optional): whole channel block
  (`{"ref": "chat-1", "send": "..." }`), or `null`.
A connector with an `events` block runs a supervisor; changing `events` while it
runs restarts it, and changing only `actions` does not.

### delete
Remove one connector and stop its event stream. Destructive.
- `name` (string, required): connector name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

### connect
Start one connector process.
- `name` (string, required): connector name.

### disconnect
Stop one connector process.
- `name` (string, required): connector name.

### restart
Restart one connector process.
- `name` (string, required): connector name.

### test
Send one test event through a connector.
- `name` (string, required): connector name.
- `event` (any, optional): event payload.

### logs
Read the log tail of one connector. Follow mode is not available; poll instead.
The response is `{"lines": [...]}`, newest last, with `truncated: true` when `lines` cut it.
- `name` (string, required): connector name.
- `lines` (number, optional): keep only the last N lines.
- `max_bytes` (number, optional): cap the body at this size; default 64 KB. A cut body returns `{"content": "…", "truncated": true}`.

### cursor_show
Read the event cursor of one connector.
- `name` (string, required): connector name.

### cursor_set
Set the event cursor of one connector.
- `name` (string, required): connector name.
- `last_event_id` (string, required): cursor value.

### cursor_reset
Reset the event cursor of one connector to the start.
- `name` (string, required): connector name.

## Related guides

`connectors-and-rules`
