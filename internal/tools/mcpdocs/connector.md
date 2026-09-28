# connector

Connector operations: manage connectors, their instances, their login, their rooms, their versions, and their workspace layers. Search the Trebi catalog and the MCP registries, and install from them.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

A connector is one entry in `connectors.yaml`. The entry names where the manifest comes from (`from`), the `version` and the `track`, the setup `inputs`, and an optional merge `patch` on the manifest. The daemon resolves the entry into a manifest. A connector can run one global instance (key `name`) and one instance for each workspace that changes it (key `name@workspace`). A workspace layer changes the entry for one workspace: `version`, `track`, `inputs`, `patch`, or `disabled`.

Many actions take `instance` and `workspace`. With `instance`, the action uses that instance. With only `workspace`, the action uses the instance that serves that workspace. With neither, the action uses the global instance.

## Actions

### list
List connectors. Includes managed rows from `connectors.yaml`, the native rows, and read-only **inherited** MCP servers from user harness config. Inherited rows carry `provenance: inherited`, `managed: false`, `origins[]` (tool, path, key), and `injected` (whether a trebi session of that harness loads them).
A managed row carries `name`, `from`, `version`, `track`, `title`, `description`, `publisher`, `capabilities` (`actions`, `events`, `channel`), `inputs`, `features`, and `instances`. Each instance row has `key`, `workspace`, `status`, `pid`, `restarts`, `last_err`, `last_event_id`, `last_event_at`, and `counters{events,duplicates,parse_errors}`. `status` is `stopped`, `starting`, `listening`, `polling`, `backoff`, or `crashed`. A running protocol connector also carries `auth{state,reason,message,account}`. A row that does not resolve carries `resolve_error`.
When `connectors.yaml` does not load, the reply carries `load_error` with the parse error. The daemon then refuses every connector write with `conflict` until the file loads again. Fix the file by hand.
A row shows each literal input value as `***`. A value that holds a `secret:NAME` reference stays visible. A write that sends `***` back keeps the stored value.
- `limit` (number, optional): page size.
- `capability` (string, optional): keep rows with `actions`, `events`, or `channel`.
- `provenance` (string, optional): keep rows with `native`, `managed`, or `inherited`.
- `workspace` (string, optional): resolve each row with the layer of this workspace.

### show
Read one connector by name or `id`. Duplicate names return `ambiguous`; use `id`.
- `name` (string, required): connector name or id.
- `workspace` (string, optional): resolve the row with the layer of this workspace, and show the instance that serves it.

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
Install one catalog entry as a connector.
A Trebi catalog entry (`registry: trebi`) installs its signed snapshot: the manifest and the skill, then the program of its `install` block (a GitHub release with a checksum, Homebrew, or `go install`). The entry follows `track`; the default is `patch`. `as`, `package`, and `remote` return `validation` for a Trebi entry.
An MCP registry entry becomes an inline entry with an `actions.mcp` block. `track` returns `validation` for it. **This runs third-party code on this machine** (through npx, uvx, docker, or the entry `runtimeHint`) when a run uses the connector. A package must use the `stdio` transport. A remote URL must be `https` and must not name a loopback, link-local, or private address.
`inputs` fills the inputs that `catalog_entry` lists: env variables, headers, arguments, and `{name}` template variables. A value `secret:NAME` is kept as a reference. A value for a secret input goes to the secret store as `<connector>_<INPUT>`, and the entry refers to it as `secret:NAME`. When a secret with that name exists and the connector did not write it, the call returns `validation`; pass `secret:NAME` to use it. A key that no input uses returns `validation`. A required input with no value returns `validation` and names the missing inputs.
A connector name that already exists returns `conflict`. A failed install removes what it wrote.
- `name` (string, required): server name.
- `registry` (string, optional): registry name.
- `version` (string, optional): server version; empty uses the latest.
- `track` (string, optional): Trebi entries only: `exact`, `patch`, `minor`, or `latest`.
- `as` (string, optional): connector name; empty derives one from the server name.
- `package` (number, optional): package index to install.
- `remote` (number, optional): remote index to install. Give `package` or `remote`, not both (`validation`). With neither, the install uses the first package, else the first remote.
- `inputs` (object, optional): input values as `{NAME: value}`.

### versions
List the catalog versions of one connector, newest first. Each row has `version`, `published_at`, `yanked`, `installed`, `current` (the global entry uses it), and `workspaces` (the workspaces whose layer uses it).
- `name` (string, required): connector name.

### check_updates
Check the Trebi catalog for new versions now. The daemon also checks once a day. A connector on the `patch` track moves to the new version at once. On `minor` or `latest`, the connector row gets `update: {version}` and the Inbox gets an item; use `update` to apply it. `exact` never changes. The reply is `{results: [{name, from, to, applied, error}]}`.
- `name` (string, optional): connector name; empty checks every connector.

### update
Move one connector to a catalog version. The daemon installs the version next to the current one, switches the entry, restarts the instance, and removes versions that nothing uses. The reply is the connector row.
- `name` (string, required): connector name.
- `version` (string, optional): catalog version; empty takes the newest version on the track that is not yanked.
- `workspace` (string, optional): update the layer of this workspace, not the global entry.

### capabilities
Show what one connector instance offers: `capabilities`, the `actions` (each CLI command with its `path`, empty when not found; for MCP the live `tools`, `prompts`, `resources`, `server`, and `instructions`), the `events` and `channel` blocks, the `session` (the `initialize` result of a running protocol instance), and the `skill` (`name`, `description`, `path`). The daemon starts the MCP server to read its lists and keeps the result for 10 minutes. A server that does not start sets `actions.mcp.error`.
- `name` (string, required): connector name.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.

### conformance
Check a running protocol connector against the promises of its manifest (Part H of the connector protocol). The daemon uses the live instance and never stops it. The checks are `initialize`, `ping`, one `feature <name>` row for each read-only feature, `unsupported`, `send once` (only with `room`), `login` (only when the instance needs a login), and `events` (the events that history or replay return). A feature that writes to the account is skipped. The reply is `{name, instance, ok, results: [{check, status, message}]}`; `status` is `pass`, `fail`, or `skip`. An instance that does not run returns `conflict`.
- `name` (string, required): connector name.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.
- `room` (string, optional): a test room; it enables the `typing` and `send once` checks. `send once` sends a real message to that room.

### gc
Remove installed catalog versions that no entry and no workspace layer uses. The reply is `{removed, dry_run}`.
- `dry_run` (boolean, optional): list the versions only.

### create
Create one connector from an entry. The name matches `^[a-z0-9][a-z0-9-]{0,39}$`; `email`, `webhook`, and `trebi` are reserved. A catalog entry must pass the signature check and the manifest validation. An inline or file manifest must pass the same validation.
- `entry` (object, required): the entry, for example `{"name": "whatsapp", "from": "catalog", "track": "minor"}`. Keys: `name` (required); `from`: `catalog` (the Trebi catalog), `inline` (the `manifest` key holds the manifest), or `file:<path>` (a manifest file on the daemon host); `version` (a catalog version); `track`: `exact`, `patch`, `minor`, or `latest`; `inputs` (the setup values as `{NAME: value}`; a value `secret:NAME` refers to a secret); `patch` (a JSON merge patch on the manifest); `manifest` (the inline manifest).

Mutations (`create`, `replace`, `set`, `delete`, `connect`, `disconnect`, `restart`, `test`, `cursor_set`, `install`, `update`, `login`, `logout`, `bind`, and the `override_*` writes) on an inherited row return `conflict`.

### replace
Replace the whole entry of one connector (`PUT`). Use it for an import: the body is the new entry, and anything absent is gone.
- `name` (string, required): connector name.
- `entry` (object, required): the complete entry, as in `create`. The name in the path wins over a `name` key.

### set
Update one connector (`PATCH`). Each key given replaces that key of the entry; keys left out stay. A rename is not possible; use `create` and `delete`. A change that alters the resolved manifest or the inputs restarts a running instance.
- `name` (string, required): connector name.
- `version` (string, optional): catalog version.
- `track` (string, optional): `exact`, `patch`, `minor`, or `latest`.
- `inputs` (object, optional): setup values as `{NAME: value}`; replaces the stored map. To keep a stored value, send it back as `***`, as `show` returns it.
- `patch` (object, optional): JSON merge patch on the manifest, or `null` to remove it.
- `manifest` (object, optional): the inline manifest of an `inline` entry, or `null` to remove it.

### delete
Remove one connector and stop its instances. Destructive.
- `name` (string, required): connector name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

`connect`, `disconnect`, `restart`, `test`, `logs`, and the `cursor_*` actions need an `events` block. On a connector without one they return HTTP 409 with the code `validation`.

### connect
Start one connector instance.
- `name` (string, required): connector name.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.

### disconnect
Stop one connector instance.
- `name` (string, required): connector name.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.

### restart
Restart one connector instance.
- `name` (string, required): connector name.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.

### test
Send one test event through a connector. An event whose declared `id` value is already stored returns `conflict`; change the id.
- `name` (string, required): connector name.
- `event` (any, optional): event payload.

### logs
Read the log tail of one connector instance. Follow mode is not available; poll instead. The response is `{"lines": [...]}`, newest last, with `truncated: true` when `lines` cut it.
- `name` (string, required): connector name.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.
- `lines` (number, optional): keep only the last N lines.
- `max_bytes` (number, optional): cap the body at this size; default 64 KB. A cut body returns `{"content": "…", "truncated": true}`.

### cursor_show
Read the event cursor of one connector instance.
- `name` (string, required): connector name.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.

### cursor_set
Set the event cursor of one connector instance.
- `name` (string, required): connector name.
- `last_event_id` (string, required): cursor value.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.

### cursor_reset
Reset the event cursor of one connector instance to the start.
- `name` (string, required): connector name.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.

### override_show
Read the layer of one workspace for one connector. A workspace with no layer returns `not_found`. Input values show as `***`.
- `workspace` (string, required): workspace id or name.
- `name` (string, required): connector name.

### override_set
Replace the layer of one workspace for one connector. The layer is stored in the `trebi.yaml` of the workspace. A layer that changes the resolved manifest or the inputs gives the workspace its own instance (`name@workspace`). The reply is the connector row for that workspace.
- `workspace` (string, required): workspace id or name.
- `name` (string, required): connector name.
- `version` (string, optional): catalog version for this workspace.
- `track` (string, optional): `exact`, `patch`, `minor`, or `latest`.
- `inputs` (object, optional): setup values for this workspace as `{NAME: value}`. A value `secret:NAME` refers to a secret.
- `patch` (object, optional): JSON merge patch on the manifest for this workspace.
- `disabled` (boolean, optional): true turns the connector off in this workspace.

### override_delete
Remove the layer of one workspace for one connector. The workspace then uses the global instance.
- `workspace` (string, required): workspace id or name.
- `name` (string, required): connector name.

### login
Start a login flow on one connector instance. The instance must run. The reply is `{flow_id, instance, step, done}`. `step.kind` is `qr` (`data` is the QR payload), `device_code` (`code` and `url`), `url` (open `url`), `input` (`fields[]` as `{name, label, secret, help}`; answer with `login_submit`), or `wait`. A step can carry `message` and `expires_at`. `done` is `{ok, account, error}` when the flow ends. Login steps never go to the bus or to a log. The UI and `trebi connector login` follow the flow live; an MCP caller reads the next step from the reply of `login_submit`, or calls `show` and reads `auth.state`.
- `name` (string, required): connector name.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.
- `kind` (string, optional): the login kind to ask for, for example `qr`; empty lets the connector choose.

### login_submit
Answer an `input` step of a login flow. The reply is the current frame `{flow_id, step, done}`.
- `name` (string, required): connector name.
- `flow_id` (string, required): flow id from `login`.
- `fields` (object, optional): the answers as `{name: value}`.

### login_cancel
Stop a login flow. The reply is the last frame, with `done.error: "cancelled"`.
- `name` (string, required): connector name.
- `flow_id` (string, required): flow id from `login`.

### logout
End the session of one connector instance. The connector then needs a new `login`.
- `name` (string, required): connector name.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.

### rooms
List the rooms of a channel connector: chats, groups, channels, or mailboxes. The reply is `{instance, rooms, next_cursor}`. Each room has `id`, `name`, `kind`, `parent`, and `last_activity_at`. For `email`, the rooms are the Trebi addresses.
- `name` (string, required): connector name.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.
- `q` (string, optional): substring match on the room name.
- `kind` (string, optional): keep rooms of this kind.
- `cursor` (string, optional): opaque cursor from a prior page (`next_cursor`).
- `limit` (number, optional): page size.

### threads
List the threads of one room. The reply is `{instance, threads, next_cursor}`. Each thread has `id`, `title`, `last_activity_at`, and `participants`. A connector that cannot list threads returns the threads that the daemon saw.
- `name` (string, required): connector name.
- `room` (string, required): room id from `rooms`.
- `instance` (string, optional): instance key, `name` or `name@workspace`.
- `workspace` (string, optional): use the instance that serves this workspace.
- `cursor` (string, optional): opaque cursor from a prior page (`next_cursor`).
- `limit` (number, optional): page size.

### bind
Bind one room, or one thread of a room, to the assistant or to an agent of a workspace. New messages in that room then go to that operator. The connector must be a channel. The reply is the conversation binding.
- `name` (string, required): connector name.
- `room` (string, required): room id from `rooms`.
- `workspace` (string, required): workspace id or name.
- `operator` (string, optional): `assistant` (the default) or `agents/<name>`.
- `thread` (string, optional): thread id from `threads`; empty binds the whole room.
- `instance` (string, optional): instance key, `name` or `name@workspace`.

## Related guides

`connectors-and-rules`
