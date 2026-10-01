---
topic: connectors-and-rules
title: Connectors, rules, events, and the queue
description: Create and connect connectors, test and fire rules, event triggers, and the dispatch queue.
---

# Connectors, rules, events, and the queue

A connector is one external system. It can give **actions** to an agent run (an MCP server, or CLI commands plus a skill), **events** to a job trigger, and a **channel** for a conversation. The three blocks are optional and independent; a connector with no block is invalid. See `trebi guide jobs-and-scheduling` for the `triggers[]` shape.

A connector type is a package from the catalog, for example `whatsapp`. A **connection** is one account of a type that the user adds. A type can have many connections, for example a work phone and a home phone. Each connection has a stable id (`con_` plus a ULID), a **slug**, and a **title**. Jobs, agents, triggers, and notify steps refer to the slug. The title is the name that the user sees. Each command that takes a connector accepts the slug or the id. The native connections `email` and `webhook` use the same word as id, slug, and type.

Each connection has one folder that holds its record (`connection.yaml`), its own secrets, its event cursor, and the state of its adapter. One connection runs one process.

`trebi connector list` also shows **inherited** MCP servers discovered from the user harness configs the bridge injects (`~/.claude.json` / `~/.claude/mcp.json`, `~/.codex/mcp.json`, `~/.cursor/mcp.json`, `~/.pi/agent/mcp.json`). Those rows are read-only: connect, edit, and delete are refused. Edit the origin file instead. Managed rows are the connections.

When the `connection.yaml` of a connection does not load, its row shows `load_error` and `trebi doctor` reports `connection_invalid`. Every write to that connection then returns `conflict`. Fix the file by hand.

## Connectors

```bash
trebi connector list --capability events --workspace reports --json
trebi connector show slack --workspace reports --json
trebi connector add whatsapp --slug wa-work --title "Work phone"
trebi connector apply --file ./feed.yaml
trebi connector set feed --title "Work feed"
trebi connector set feed --slug news
trebi connector set feed --input FEED_TOKEN=secret:feed_token
trebi connector set feed --patch-file ./feed-patch.yaml
trebi connector set feed --manifest-file ./feed-manifest.yaml
trebi connector test feed --event '{"id":"t1","text":"hello"}'
trebi connector connect feed
trebi connector disconnect feed
trebi connector restart feed
trebi connector logs feed
trebi connector delete feed --force
```

`connector add <type>` adds a connection of a catalog type. Run it again with a new `--slug` for a second account. With no `--slug`, the slug is the type, then `<type>-2`, `<type>-3`, and so on.

A connection has `type`, `slug`, `title`, `from` (where the manifest comes from), `version`, `track`, `inputs`, an optional merge `patch`, and, for `from: inline`, the whole `manifest`. `connector apply --file` reads one connection. It looks up the slug: when no connection has it, apply creates one with `POST`; else apply changes it with `PATCH`:

```yaml
type: feed
slug: feed
title: Feed
from: inline
inputs:
  FEED_TOKEN: secret:feed_token
manifest:
  schema: trebi-connector/1
  name: feed
  title: Feed
  description: New items from the feed.
  version: 1.0.0
  events:
    command: feed-fetch --since "$TREBI_LAST_EVENT_ID"
    format: json
    poll: 15m
    id: id
    identity: author
```

A manifest needs `schema`, `name`, `title`, `description`, `version`, and at least one of `actions` (`mcp` or `cli`), `events`, and `channel`. Each input is an env variable of the connector processes, so `${FEED_TOKEN}` in an MCP `env` value reads the input. `connector show` prints each literal input, MCP env value, and header as `***`; a `secret:NAME` reference stays visible. A plain value for a secret input of the manifest goes to the folder of the connection.

`connector set` sends a `PATCH`. `--title` sets the display name that the UI shows; an empty title restores the catalog title. `--slug` changes the slug. It also changes each reference to the old slug: the workspace layers, the `connectors:` lists and trigger `event:` values of jobs and agents, and the notify `via:` steps. When one file cannot change, the slug stays and no file changes. `--input` merges into the stored inputs. `--patch-file` replaces the merge patch, `--manifest-file` replaces the inline manifest, and a file that holds `null` removes it. With `--manifest-file`, `set` creates a connection that does not exist. A slug matches `^[a-z0-9][a-z0-9-]{0,39}$` and does not start with `con_`; `email`, `webhook`, and `trebi` are reserved.

The events block has two protocols. `lines` (the default) reads one event per output line: `format` is `json` or `line`, `poll` runs the command again on an interval (at least `10s`), and `catchup.command` reads the events after `$TREBI_LAST_EVENT_ID` at start. Catch-up needs `format: json` and `id`, and it cannot be combined with `poll`. `trebi-connector/1` runs an adapter that speaks the connector protocol: it sends typed events, and it can log in to an account and hold a channel. A `channel` block needs `events.protocol: trebi-connector/1`.

A trigger `allowlist` checks only the value at the events `identity` path. A connector with no identity fails every allowlist. The `email` connector compares the sender address without case. Every other connector compares the value exactly.

A trigger `from` (`people`, `bots`, or `anyone`, the default) reads `sender.bot` of a `trebi-connector/1` event. A `lines` connector and an adapter that does not send `sender.bot` make every sender a person. A trigger `contains` tests the event `text`: what a person sees, with the readable parts of embeds and other rich content, one per line.

A job or agent receives connector actions through `connectors:` (or the agent `kit: connectors`). A job list that is not empty replaces the kit list. `kit.mcp` is gone: trebi ignores it and doctor reports `kit_mcp_removed`; move the slugs to `kit.connectors`. A run with an MCP action needs a supported harness and runtime: claude, codex, and opencode on either runtime, and the ACP harnesses (copilot, cursor, gemini) on the CLI runtime. The CLI runtime asks the installed harness if it can take MCP servers; pi cannot. An unsupported pair fails the launch with `validation`, and doctor reports `kit_actions_unsupported` first. A resumed run keeps the actions of the run it resumes. Codex gets MCP secrets through the environment, never on the command line. For opencode, the injected servers merge into the `OPENCODE_CONFIG_CONTENT` of the job; an injected server wins on a name clash.

`connector logs` (and `--follow`) blocks — **local only**; remote: poll `connector.logs`. `connector delete` is destructive — ask first.

## Workspace overrides

A workspace can turn a connection off for its own jobs, or patch its `actions` and `skill` blocks. The workspace layer holds only `patch` and `disabled`. It does not start a second process. A workspace that needs a different account uses a second connection.

```bash
trebi connector override show slack --workspace reports
trebi connector override set slack --workspace reports --disabled
trebi connector override set slack --workspace reports --patch-file ./reports-patch.yaml
trebi connector override delete slack --workspace reports
```

`override delete` removes the layer, and the workspace uses the connection with no change.

## Accounts and rooms

A `trebi-connector/1` adapter can log in to an account and list the rooms of its channel (chats, groups, mailboxes).

```bash
trebi connector login whatsapp
trebi connector logout whatsapp
trebi connector rooms whatsapp --query ops --json
trebi connector threads whatsapp <room> --json
trebi connector senders whatsapp --query ana --json
trebi connector dependents whatsapp --json
trebi connector bind whatsapp <room> --workspace reports --operator assistant
```

`connector login` prints each step (a code, a QR code, or a link) and waits for the user. Ask the user before you log out. `connector senders` lists the recent senders from the stored events; put an `identity` in the `allowlist` of an event trigger, and a room `id` in its `rooms`. `connector dependents` lists the triggers on the connector (disabled and inactive ones too) and the rooms that are bound to the assistant or an agent. `connector threads` answers 400 `unsupported` when the connector has no `threads` feature. `connector bind` sends the messages of a room to the assistant or to `agents/<name>` of a workspace; `--thread` binds one thread only. A notify route step names the connection slug and a room id: `trebi config set notify.route '[{"via":"whatsapp","to":"<room>"},{"via":"email"}]'`.

## Conversations

A job trigger with `replies: continue` keeps each conversation of a connector in one run session. The daemon finds the conversation root of each `trebi-connector/1` event: the thread, else the message that the event quotes or answers, else the whole DM. Per platform:

- Discord and Slack: the thread. A thread id is the id of its start message, so the start message and its replies are one conversation.
- Telegram: the topic, or the reply chain.
- WhatsApp: the reply chain, or the whole DM.
- Email: the thread root.

A reply chain needs the `replies` channel feature: the adapter sets `reply_to` on a quoted reply. A follow-up run reads older messages with the history command of the connector (for example `discord-cli message list <channel_id>`). The daemon puts the connection, room, and thread in the prompt.

## Registry

Install a connector from a registry. `search` reads every registry in `connectors.registries` in order. The default list is `trebi` (the signed Trebi catalog), `official` (the MCP Registry), and `docker`, so a Trebi entry shows first.

A registry entry has a `kind`: `trebi` (the Trebi catalog), `mcp-registry` (the default; the official MCP Registry API), `smithery` (registry.smithery.ai), or `docker` (the Docker MCP Catalog file). Each search row carries `trust`: `official` for the official registry and for a Trebi catalog entry that Trebi publishes, `verified` for another Trebi catalog entry, for a Smithery server that Smithery verified, and for every Docker entry, else `community`. A registry that does not answer shows its last good rows and a warning on stderr. Smithery installs only its HTTP connections. A Docker `server` installs as `docker run`; a Docker `remote` installs as an HTTP action; a `poci` entry does not install.

```bash
trebi config set connectors.registries '[{"name":"official","url":"https://registry.modelcontextprotocol.io"},{"name":"smithery","url":"https://registry.smithery.ai","kind":"smithery"},{"name":"docker","url":"https://desktop.docker.com/mcp/catalog/v3/catalog.yaml","kind":"docker"}]'
```

```bash
trebi connector search postgres --registry official --limit 20
trebi connector search postgres --cursor <next_cursor>
trebi connector entry acme/weather --registry official --version 1.2.0
trebi connector install io.github.modelcontextprotocol/server-github --slug github --input GITHUB_PERSONAL_ACCESS_TOKEN=secret:github_pat
trebi connector install acme/weather --package 0 --input WEATHER_API_KEY=secret:weather
trebi connector install acme/weather --remote 0
```

The install adds an inline connection: the manifest has one MCP action, and each input of the server becomes an input of the connection. `--input NAME=VALUE` fills an input: an env variable, a header, an argument, or a `{name}` URL or header variable. `connector entry` (MCP `connector.catalog_entry`) lists the packages, the remotes, and the inputs of each; a `*` marks a required input. `search` prints `more: --cursor X` on stderr when a next page exists. `--input VAR=secret:NAME` keeps the reference and writes no secret. A plain value for a secret input goes to the folder of the connection. `connector delete` deletes the folder and its secrets. `--package` and `--remote` select one entry of the server; give one, not both (default: the first package, else the first remote that speaks `streamable-http` or `sse`). An install runs third-party code on this machine, so ask the user before you install.

## Versions

A Trebi catalog entry installs its signed snapshot (the manifest and the skill) and its program. It follows an update track: `exact` (never changes), `patch` (the default; moves to a new patch version at once), `minor`, or `latest`. On `minor` and `latest`, a new version shows as `update` on the row and as an Inbox item; apply it with `connector update`. The daemon checks once a day.

```bash
trebi connector install whatsapp --registry trebi --track minor
trebi connector versions whatsapp
trebi connector check-updates            # every connection; give a slug for one
trebi connector update whatsapp          # the newest version on the track
trebi connector update whatsapp --version 0.6.0
trebi connector capabilities github      # commands, MCP tools, events, channel features, skill
trebi connector gc --dry-run
```

`connector conformance <slug>` checks a running protocol connector against its manifest; `--room` adds a real test send, and `--suite` limits the suites. `connector conformance --command CMD --manifest FILE` checks an adapter program on this machine with no daemon.

`update` installs the version next to the current one, switches the connection, restarts its process, and removes versions that nothing uses. `capabilities` starts an MCP server to list its tools, prompts, and resources, and keeps the result for 10 minutes. `gc` removes installed versions that no connection uses.

## Cursors

Cursors track how far a connector has consumed.

```bash
trebi connector cursor show slack
trebi connector cursor set slack <last-event-id>
trebi connector cursor reset slack
```

## Rules

Test routing rules against a sample event before you put them in a job.

```bash
echo '{"channel":"ops","text":"deploy prod"}' | trebi rules test --match "e.channel === 'ops'" --extract "({t: e.text})" --json
trebi rules fire --connector slack --job my-job --workspace reports --match "e.channel === 'ops'" --event-file ./event.json
```

`--event-file` is **local only**; remote: pass `event` in input. `rules test` only evaluates. `rules fire` evaluates and enqueues on a match.

Test a trigger against stored events. The checks run in trigger order: `--room` (the rooms the trigger takes), `--allowlist` (the senders), `--from` (`people`, `bots`, or `anyone`), `--contains` (repeatable; the text must hold one entry), extract, then match. `--match` is optional. The result names the `stage` that stopped the event: `room`, `sender`, `from`, `contains`, `extract`, or `match`. On a match, `vars` holds the built-in vars of the event plus the extract vars. `--replies continue` sets `conversation_key` to the key of a job with `replies: continue`. Add `--job` (and `--workspace`) with `--replies continue` to test as that job: an event in an open conversation of the job gets `follow_up` true and skips `from`, `contains`, and match. A bot message in an open conversation stops at `from`. Without `--job`, the test treats each event as the first message of its conversation.

```bash
trebi rules test --event-id <event-id> --room 120363430107848596@g.us --match "event.type === 'message'" --json
trebi rules test --event-id <event-id> --from bots --contains failure --replies continue --json
trebi rules test-batch <event-id> <event-id> --allowlist 15551234567@s.whatsapp.net --json
```

`rules test-batch` takes at most 50 events.

## Hosted webhooks

A hosted webhook is a public URL the cloud receives and pushes over the link socket. Mint one per sender. The first path segment of the posted path becomes the event `label`, so one hook fans out to many jobs by path.

```bash
trebi hooks create --workspace reports --label deploy
trebi hooks list
trebi hooks delete <id>
```

`hooks create` prints the URL once. Handle it like a webhook secret: anyone holding it can post an event. `hooks delete` is destructive — ask first. `hooks list` masks the URLs; `--reveal` prints them and writes an audit entry.

A job fires from one delivery. `event.headers` carries the sender signature allowlist, so `match` can verify the sender:

```yaml
triggers:
  - event: webhook
    match: event.path === "/deploy" && event.headers["x-github-event"] === "push"
```

`extract` pulls values out of the payload. It runs before `match` and rebinds `event` to its result, so a rule that extracts reads the extracted fields in `match`:

```yaml
triggers:
  - event: webhook
    extract: "({ ref: json(event.body).ref })"
    match: event.ref === "refs/heads/main"
```

The payload is `hook_id`, `label`, `method`, `path`, `content_type`, `headers`, `body`, and `received_at`. `body` stays a string: call `json(event.body)` for a value. Delivery is at-least-once; a duplicate delivery id never fires twice.

## Durable trigger events

```bash
trebi event list --limit 50 --json
trebi event retrigger <id>
```

`event retrigger` matches a stored event against the current triggers again. It writes no new event row and works for every connector, `email` and `webhook` included.

## Queue

The dispatch queue of pending launches.

```bash
trebi queue --status pending --json
trebi queue cancel <id> [id...]
trebi queue requeue <id>
```

`queue requeue` moves a blocked/done row back to pending.
