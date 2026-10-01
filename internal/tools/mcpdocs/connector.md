# connector

Connector operations: manage connections, their login, their rooms, their versions, and their workspace layers. Search the Trebi catalog and the MCP registries, and install from them.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

A connector type is a package from the catalog, for example `whatsapp`. A connection is one account of a type that the user adds. A type can have many connections, for example a work phone and a home phone. Each connection has a stable `id` (`con_` plus a ULID), a `slug`, and a `title`. The id never changes. Jobs, agents, triggers, and notify steps refer to the slug. The title is the name that the user sees. The native connections `email` and `webhook` use the same word as id, slug, and type.

Each connection has one folder that holds its record (`from`, `version`, `track`, `inputs`, and an optional merge `patch` on the manifest), its own secrets, its event cursor, and the state of its adapter. One connection runs one process. A workspace layer can turn a connection off in one workspace, or patch its `actions` and `skill` for that workspace. It does not start a second process. A workspace that needs a different account uses a second connection.

Each action that takes `connector` accepts the slug or the id. A slug can change, so keep the id when you store a reference outside Trebi.

## Actions

### list
List connectors. Includes the managed connections, the native rows, and read-only **inherited** MCP servers from user harness config. Inherited rows carry `provenance: inherited`, `managed: false`, `origins[]` (tool, path, key), and `injected` (whether a trebi session of that harness loads them).
A managed row carries `id`, `type`, `slug`, `title`, `from`, `version`, `track`, `description`, `publisher`, `capabilities` (`actions`, `events`, `channel`), `inputs`, `features`, and `folder`. A connection with an `events` block also carries `status`, `pid`, `restarts`, `last_err`, `last_event_id`, `last_event_at`, and `counters{events,duplicates,parse_errors}`. `status` is `stopped`, `starting`, `listening`, `polling`, `backoff`, or `crashed`. A running protocol connector also carries `auth{state,reason,message,account}`. A row that does not resolve carries `resolve_error`. A connection folder that does not load carries `load_error`; fix its `connection.yaml` by hand.
A row shows each literal input value as `***`. A value that holds a `secret:NAME` reference stays visible. A write that sends `***` back keeps the stored value.
- `limit` (number, optional): page size.
- `capability` (string, optional): keep rows with `actions`, `events`, or `channel`.
- `provenance` (string, optional): keep rows with `native`, `managed`, or `inherited`.
- `workspace` (string, optional): resolve each row with the layer of this workspace.

### show
Read one connection.
- `connector` (string, required): connection slug or id.
- `workspace` (string, optional): resolve the row with the layer of this workspace.

### search
Search the configured registries for installable connectors. With no `registry` the action reads every configured registry in order and merges the pages. The default order is `trebi` (the signed Trebi catalog), `official` (the MCP registry), then `docker`, so a Trebi entry shows first. The reply carries `entries`, `registries` (the configured list as `[{name, url, kind}]`) and, when more exist, `next_cursor`. Each entry has `registry`, `name`, `version`, and `trust`: `official`, `verified`, or `community`. A Trebi catalog entry also has `publisher`, `capabilities`, `versions`, and `icon_url`. An MCP entry has `packages` and `remotes`. `kind` is `trebi`, `mcp-registry`, `smithery`, or `docker`. A registry that fails does not fail the search: the reply lists it in `registry_errors: [{registry, error}]` and the other registries still answer. A registry that fails after a good search in the last 10 minutes returns that page again and shows in `stale_registries`. The search fails only when every registry fails.
- `q` (string, optional): substring match on the server name.
- `registry` (string, optional): one configured registry name; empty reads all.
- `limit` (number, optional): page size per registry.
- `cursor` (string, optional): opaque cursor from a prior page (`next_cursor`).

### catalog_entry
Read one server entry and the inputs that `install` accepts. `package_inputs[i]` and `remote_inputs[i]` list the inputs of package `i` and remote `i` as `{name, kind, description, format, default, choices, required, secret}`. `kind` is `env`, `header`, `argument`, or `variable`. The `name` is the key to pass in the install `inputs`.
- `name` (string, required): server name, for example `ai.adtest/adtest-mcp`.
- `registry` (string, optional): registry name.
- `version` (string, optional): server version; empty asks for the latest.

### install
Install one catalog entry as a new connection. The reply is the connection row.
A Trebi catalog entry (`registry: trebi`) installs its signed snapshot: the manifest and the skill, then the program of its `install` block (a GitHub release with a checksum, Homebrew, or `go install`). The connection follows `track`; the default is `patch`. `package` and `remote` return `validation` for a Trebi entry.
An MCP registry entry becomes an inline connection with an `actions.mcp` block. `track` returns `validation` for it. **This runs third-party code on this machine** (through npx, uvx, docker, or the entry `runtimeHint`) when a run uses the connector. A package must use the `stdio` transport. A remote URL must be `https` and must not name a loopback, link-local, or private address.
`inputs` fills the inputs that `catalog_entry` lists: env variables, headers, arguments, and `{name}` template variables. A value `secret:NAME` is kept as a reference to the secret store. A value for a secret input goes to the folder of the connection. A key that no input uses returns `validation`. A required input with no value returns `validation` and names the missing inputs.
A slug that another connection uses returns `conflict`. A failed install removes what it wrote. To add a second account of the same type, install again with a new `slug`.
- `name` (string, required): server name, for example `whatsapp`.
- `registry` (string, optional): registry name.
- `version` (string, optional): server version; empty uses the latest.
- `track` (string, optional): Trebi entries only: `exact`, `patch`, `minor`, or `latest`.
- `slug` (string, optional): slug of the new connection. It matches `^[a-z0-9][a-z0-9-]{0,39}$`. Empty uses the type, then `<type>-2`, `<type>-3`, and so on.
- `title` (string, optional): display name, for example `Work phone`.
- `package` (number, optional): package index to install.
- `remote` (number, optional): remote index to install. Give `package` or `remote`, not both (`validation`). With neither, the install uses the first package, else the first remote.
- `inputs` (object, optional): input values as `{NAME: value}`.

### versions
List the catalog versions of the type of one connection, newest first. Each row has `version`, `published_at`, `yanked`, `installed`, `current` (this connection uses it), and `connections` (the slugs of the connections of the type that use it).
- `connector` (string, required): connection slug or id.

### check_updates
Check the Trebi catalog for new versions now. The daemon also checks once a day. A connection on the `patch` track moves to the new version at once. On `minor` or `latest`, the connection row gets `update: {version}` and the Inbox gets an item; use `update` to apply it. `exact` never changes. The reply is `{results: [{id, slug, from, to, applied, error}]}`.
- `connector` (string, optional): connection slug or id; empty checks every connection.

### update
Move one connection to a catalog version. The daemon installs the version next to the current one, switches the connection, restarts its process, and removes versions that nothing uses. The reply is the connection row.
- `connector` (string, required): connection slug or id.
- `version` (string, optional): catalog version; empty takes the newest version on the track that is not yanked.

### capabilities
Show what one connection offers: `capabilities`, the `actions` (each CLI command with its `path`, empty when not found; for MCP the live `tools`, `prompts`, `resources`, `server`, and `instructions`), the `events` and `channel` blocks, the `session` (the `initialize` result of a running protocol connector), and the `skill` (`name`, `description`, `path`). The daemon starts the MCP server to read its lists and keeps the result for 10 minutes. A server that does not start sets `actions.mcp.error`.
- `connector` (string, required): connection slug or id.
- `workspace` (string, optional): apply the `actions` and `skill` patch of this workspace.

### conformance
Check a running protocol connector against the promises of its manifest (Part H of the connector protocol). The daemon uses the live process and never stops it. The reply is `{id, slug, ok, results: [{suite, check, status, message}]}`; `status` is `pass`, `fail`, or `skip`. The suites are `manifest`, `install`, `inputs`, `actions.cli`, `actions.mcp`, `events.lines`, `protocol`, and `folder`. The `protocol` suite checks `initialize`, `ping`, one `feature <name>` row for each read-only feature, `unsupported`, `send once` (only with `room`), `login` (only when the connection needs a login), and `events` (the events that history or replay return). A feature that writes to the account is skipped. A connection that does not run returns `conflict`.
- `connector` (string, required): connection slug or id.
- `room` (string, optional): a test room; it enables the `typing` and `send once` checks. `send once` sends a real message to that room.
- `suites` (array, optional): the suites to run; empty runs all of them.

### gc
Remove installed catalog versions that no connection uses. The reply is `{removed, dry_run}`.
- `dry_run` (boolean, optional): list the versions only.

### create
Create one connection. A catalog connection must pass the signature check and the manifest validation. An inline or file manifest must pass the same validation. The reply is the connection row with its new `id`.
- `connection` (object, required): the connection, for example `{"type": "whatsapp", "slug": "whatsapp-work", "title": "Work phone", "from": "catalog", "track": "minor"}`. Keys: `type` (required; the connector type); `slug` (empty uses the default slug of the type); `title`; `from`: `catalog` (the Trebi catalog), `inline` (the `manifest` key holds the manifest), or `file:<path>` (a manifest file on the daemon host); `version` (a catalog version); `track`: `exact`, `patch`, `minor`, or `latest`; `inputs` (the setup values as `{NAME: value}`; a value `secret:NAME` refers to a secret); `patch` (a JSON merge patch on the manifest); `manifest` (the inline manifest). The slug matches `^[a-z0-9][a-z0-9-]{0,39}$`, and must not start with `con_`. `email`, `webhook`, and `trebi` are reserved.

Mutations (`create`, `set`, `delete`, `connect`, `disconnect`, `restart`, `test`, `cursor_set`, `install`, `update`, `login`, `logout`, `bind`, and the `override_*` writes) on an inherited row return `conflict`.

### set
Update one connection (`PATCH`). Each key given replaces that key of the connection; keys left out stay. A change that alters the resolved manifest or the inputs restarts a running process.
A new `slug` also rewrites each reference to the old slug: the workspace layers in `trebi.yaml`, the `connectors:` lists and trigger `event:` values of jobs and agents, and the notify `via:` steps in `config.yaml`. The reply lists them in `changed_files`. When one file cannot change, the daemon puts back the files that it changed, keeps the old slug, and returns `conflict`.
- `connector` (string, required): connection slug or id.
- `slug` (string, optional): new slug.
- `title` (string, optional): display name, at most 60 characters on one line. An empty string restores the catalog title.
- `version` (string, optional): catalog version.
- `track` (string, optional): `exact`, `patch`, `minor`, or `latest`.
- `inputs` (object, optional): setup values as `{NAME: value}`; replaces the stored map. To keep a stored value, send it back as `***`, as `show` returns it.
- `patch` (object, optional): JSON merge patch on the manifest, or `null` to remove it.
- `manifest` (object, optional): the inline manifest of an `inline` connection, or `null` to remove it.

### delete
Remove one connection, stop its process, and delete its folder. Destructive.
- `connector` (string, required): connection slug or id.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

`connect`, `disconnect`, `restart`, `test`, `logs`, and the `cursor_*` actions need an `events` block. On a connection without one they return HTTP 409 with the code `validation`.

### connect
Start the process of one connection.
- `connector` (string, required): connection slug or id.

### disconnect
Stop the process of one connection.
- `connector` (string, required): connection slug or id.

### restart
Restart the process of one connection.
- `connector` (string, required): connection slug or id.

### test
Send one test event through a connection. An event whose declared `id` value is already stored returns `conflict`; change the id.
- `connector` (string, required): connection slug or id.
- `event` (any, optional): event payload.

### logs
Read the log tail of one connection. Follow mode is not available; poll instead. The response is `{"lines": [...]}`, newest last, with `truncated: true` when `lines` cut it.
- `connector` (string, required): connection slug or id.
- `lines` (number, optional): keep only the last N lines.
- `max_bytes` (number, optional): cap the body at this size; default 64 KB. A cut body returns `{"content": "…", "truncated": true}`.

### cursor_show
Read the event cursor of one connection. The reply is `{id, slug, last_event_id, updated_at}`.
- `connector` (string, required): connection slug or id.

### cursor_set
Set the event cursor of one connection.
- `connector` (string, required): connection slug or id.
- `last_event_id` (string, required): cursor value.

### cursor_reset
Reset the event cursor of one connection to the start.
- `connector` (string, required): connection slug or id.

### override_show
Read the layer of one workspace for one connection. The reply is `{workspace, slug, override}`. A workspace with no layer returns `not_found`.
- `workspace` (string, required): workspace id or name.
- `connector` (string, required): connection slug or id.

### override_set
Replace the layer of one workspace for one connection. The layer is stored in the `trebi.yaml` of the workspace under the slug. The layer does not start a second process. The reply is the connection row for that workspace.
- `workspace` (string, required): workspace id or name.
- `connector` (string, required): connection slug or id.
- `patch` (object, optional): JSON merge patch on the `actions` and `skill` blocks for this workspace.
- `disabled` (boolean, optional): true turns the connection off in this workspace.

### override_delete
Remove the layer of one workspace for one connection. The workspace then uses the connection with no change.
- `workspace` (string, required): workspace id or name.
- `connector` (string, required): connection slug or id.

### login
Start a login flow on one connection. The process must run. The reply is `{flow_id, id, step, done}`. `step.kind` is `qr` (`data` is the QR payload), `device_code` (`code` and `url`), `url` (open `url`), `input` (`fields[]` as `{name, label, secret, help}`; answer with `login_submit`), or `wait`. A step can carry `message` and `expires_at`. `done` is `{ok, account, error}` when the flow ends. Login steps never go to the bus or to a log. The UI and `trebi connector login` follow the flow live; an MCP caller reads the next step from the reply of `login_submit`, or calls `show` and reads `auth.state`.
- `connector` (string, required): connection slug or id.
- `kind` (string, optional): the login kind to ask for, for example `qr`; empty lets the connector choose.

### login_submit
Answer an `input` step of a login flow. The reply is the current frame `{flow_id, step, done}`.
- `connector` (string, required): connection slug or id.
- `flow_id` (string, required): flow id from `login`.
- `fields` (object, optional): the answers as `{name: value}`.

### login_cancel
Stop a login flow. The reply is the last frame, with `done.error: "cancelled"`.
- `connector` (string, required): connection slug or id.
- `flow_id` (string, required): flow id from `login`.

### logout
End the session of one connection. The connection then needs a new `login`.
- `connector` (string, required): connection slug or id.

### rooms
List the rooms of a channel connection: chats, groups, channels, or mailboxes. The reply is `{id, rooms, next_cursor}`. Each room has `id`, `name`, `kind`, `parent`, and `last_activity_at`. For `email`, the rooms are the Trebi addresses.
- `connector` (string, required): connection slug or id.
- `q` (string, optional): substring match on the room name.
- `kind` (string, optional): keep rooms of this kind.
- `cursor` (string, optional): opaque cursor from a prior page (`next_cursor`).
- `limit` (number, optional): page size.

### senders
List the recent senders of a connection, from its stored events, the most recent first. Use an `identity` as an entry of a trigger `allowlist`. The reply is `{id, senders}`. Each sender has `identity`, `name`, `last_at`, and `count` (the number of stored events).
- `connector` (string, required): connection slug or id, or `email`.
- `q` (string, optional): substring match on the identity or the name.
- `limit` (number, optional): at most this many senders (default 50, at most 200).

### dependents
List what uses a connection: each event trigger on it and each room binding. The reply is `{id, slug, triggers, bindings}`. Each trigger has `kind` (job, agent, or assistant), `workspace`, `name`, `on_index`, `disabled`, `active`, `rooms` (empty means any room), and `senders` (the size of the allowlist). Disabled triggers and inactive jobs are in the list; they do not fire. Each binding has `workspace`, `operator` (assistant or agents/<name>), `room`, `thread`, and `conversation_id`.
- `connector` (string, required): connection slug or id, or `email`.

### threads
List the threads of one room. The reply is `{id, threads, next_cursor}`. Each thread has `id`, `title`, `last_activity_at`, and `participants`. A connector that cannot list threads returns the threads that the daemon saw. A connection without the `threads` feature answers 400 `unsupported`.
- `connector` (string, required): connection slug or id.
- `room` (string, required): room id from `rooms`.
- `cursor` (string, optional): opaque cursor from a prior page (`next_cursor`).
- `limit` (number, optional): page size.

### bind
Bind one room, or one thread of a room, to the assistant or to an agent of a workspace. New messages in that room then go to that operator. The connection must be a channel. The reply is the conversation binding.
- `connector` (string, required): connection slug or id.
- `room` (string, required): room id from `rooms`.
- `workspace` (string, required): workspace id or name.
- `operator` (string, optional): `assistant` (the default) or `agents/<name>`.
- `thread` (string, optional): thread id from `threads`; empty binds the whole room. A connection without the `threads` feature answers 400 `unsupported` to a thread.

## Related guides

`connectors-and-rules`
