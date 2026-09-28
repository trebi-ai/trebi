---
topic: connectors-and-rules
title: Connectors, rules, events, and the queue
description: Create and connect connectors, test and fire rules, event triggers, and the dispatch queue.
---

# Connectors, rules, events, and the queue

A connector is one external system. It can give **actions** to an agent run (an MCP server, or CLI commands plus a skill), **events** to a job trigger, and a **channel** for a conversation. The three blocks are optional and independent; a connector with no block is invalid. See `trebi guide jobs-and-scheduling` for the `triggers[]` shape.

`trebi connector list` also shows **inherited** MCP servers discovered from the user harness configs the bridge injects (`~/.claude.json` / `~/.claude/mcp.json`, `~/.codex/mcp.json`, `~/.cursor/mcp.json`, `~/.pi/agent/mcp.json`). Those rows are read-only: connect, edit, and delete are refused. Edit the origin file instead. Managed rows are the ones in `connectors.yaml`.

When `connectors.yaml` does not load, `trebi connector list` prints the error and `trebi doctor` reports `connectors_file_invalid`. Every connector write then returns `conflict`, so a write cannot erase the file. Fix the file by hand.

## Connectors

```bash
trebi connector list --capability events --workspace reports --json
trebi connector show slack --workspace reports --json
trebi connector apply --file ./feed.yaml
trebi connector set feed --input FEED_TOKEN=secret:feed_token
trebi connector set feed --patch-file ./feed-patch.yaml
trebi connector set feed --manifest-file ./feed-manifest.yaml
trebi connector test feed --event '{"id":"t1","text":"hello"}'
trebi connector connect feed
trebi connector disconnect feed
trebi connector restart feed --workspace reports
trebi connector logs feed --instance feed@reports
trebi connector delete feed --force
```

A connector is one entry in `connectors.yaml`. The entry has `from` (where the manifest comes from), `version`, `track`, `inputs`, an optional merge `patch`, and, for `from: inline`, the whole `manifest`. `connector apply --file` writes one entry with `PUT`:

```yaml
feed:
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

A manifest needs `schema`, `name`, `title`, `description`, `version`, and at least one of `actions` (`mcp` or `cli`), `events`, and `channel`. Each input is an env variable of the connector processes, so `${FEED_TOKEN}` in an MCP `env` value reads the input. `connector show` prints each literal input, MCP env value, and header as `***`; a `secret:NAME` reference stays visible. A plain value for a secret input of the manifest becomes the secret `<connector>_<INPUT>`.

`connector set` sends a `PATCH`. `--input` merges into the stored inputs. `--patch-file` replaces the merge patch, `--manifest-file` replaces the inline manifest, and a file that holds `null` removes it. With `--manifest-file`, `set` creates a connector that does not exist. A connector name matches `^[a-z0-9][a-z0-9-]{0,39}$`; `email`, `webhook`, and `trebi` are reserved.

The events block has two protocols. `lines` (the default) reads one event per output line: `format` is `json` or `line`, `poll` runs the command again on an interval (at least `10s`), and `catchup.command` reads the events after `$TREBI_LAST_EVENT_ID` at start. Catch-up needs `format: json` and `id`, and it cannot be combined with `poll`. `trebi-connector/1` runs an adapter that speaks the connector protocol: it sends typed events, and it can log in to an account and hold a channel. A `channel` block needs `events.protocol: trebi-connector/1`.

A trigger `allowlist` checks only the value at the events `identity` path. A connector with no identity fails every allowlist. The `email` connector compares the sender address without case. Every other connector compares the value exactly.

A job or agent receives connector actions through `connectors:` (or the agent `kit: connectors`). A job list that is not empty replaces the kit list. `kit.mcp` is gone: trebi ignores it and doctor reports `kit_mcp_removed`; move the names to `kit.connectors`. A run with an MCP action needs a supported harness and runtime: claude, codex, and opencode on either runtime, and the ACP harnesses (copilot, cursor, gemini) on the CLI runtime. The CLI runtime asks the installed harness if it can take MCP servers; pi cannot. An unsupported pair fails the launch with `validation`, and doctor reports `kit_actions_unsupported` first. A resumed run keeps the actions of the run it resumes. Codex gets MCP secrets through the environment, never on the command line. For opencode, the injected servers merge into the `OPENCODE_CONFIG_CONTENT` of the job; an injected server wins on a name clash.

`connector logs` (and `--follow`) blocks — **local only**; remote: poll `connector.logs`. `connector delete` is destructive — ask first.

## Workspace overrides

A workspace can change a connector for its own jobs. The workspace layer holds `version`, `track`, `inputs`, `patch`, and `disabled`. A layer that changes the connector runs a second instance, `<name>@<workspace>`. `--workspace` selects the instance of that workspace, and `--instance` names an instance key.

```bash
trebi connector override show slack --workspace reports
trebi connector override set slack --workspace reports --input SLACK_TOKEN=secret:reports_slack
trebi connector override set slack --workspace reports --disabled
trebi connector override delete slack --workspace reports
```

`override delete` removes the layer, and the workspace uses the global connector again.

## Accounts and rooms

A `trebi-connector/1` adapter can log in to an account and list the rooms of its channel (chats, groups, mailboxes).

```bash
trebi connector login whatsapp
trebi connector logout whatsapp
trebi connector rooms whatsapp --query ops --json
trebi connector threads whatsapp <room> --json
trebi connector bind whatsapp <room> --workspace reports --operator assistant
```

`connector login` prints each step (a code, a QR code, or a link) and waits for the user. Ask the user before you log out. `connector bind` sends the messages of a room to the assistant or to `agents/<name>` of a workspace; `--thread` binds one thread only. A notify route step names the connector and a room id: `trebi config set notify.route '[{"via":"whatsapp","to":"<room>"},{"via":"email"}]'`.

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
trebi connector install io.github.modelcontextprotocol/server-github --as github --input GITHUB_PERSONAL_ACCESS_TOKEN=secret:github_pat
trebi connector install acme/weather --package 0 --input WEATHER_API_KEY=secret:weather
trebi connector install acme/weather --remote 0
```

The install writes an inline entry: the manifest has one MCP action, and each input of the server becomes an entry input. `--input NAME=VALUE` fills an input: an env variable, a header, an argument, or a `{name}` URL or header variable. `connector entry` (MCP `connector.catalog_entry`) lists the packages, the remotes, and the inputs of each; a `*` marks a required input. `search` prints `more: --cursor X` on stderr when a next page exists. `--input VAR=secret:NAME` keeps the reference and writes no secret. A plain value for a secret input becomes the secret `<connector>_<VAR>`. `connector delete` deletes the secrets the install created, unless another connector refers to them. `--package` and `--remote` select one entry of the server; give one, not both (default: the first package, else the first remote that speaks `streamable-http` or `sse`). An install runs third-party code on this machine, so ask the user before you install. When a secret with the name `<connector>_<VAR>` exists and the connector did not write it, the install fails; pass `--input VAR=secret:<name>` to use it.

## Versions

A Trebi catalog entry installs its signed snapshot (the manifest and the skill) and its program. It follows an update track: `exact` (never changes), `patch` (the default; moves to a new patch version at once), `minor`, or `latest`. On `minor` and `latest`, a new version shows as `update` on the row and as an Inbox item; apply it with `connector update`. The daemon checks once a day.

```bash
trebi connector install whatsapp --registry trebi --track minor
trebi connector versions whatsapp
trebi connector check-updates            # every connector; give a name for one
trebi connector update whatsapp          # the newest version on the track
trebi connector update whatsapp --version 0.6.0 --workspace acme
trebi connector capabilities github      # commands, MCP tools, events, channel features, skill
trebi connector gc --dry-run
```

`connector conformance <name>` checks a running protocol connector against its manifest; `--room` adds a real test send. `connector conformance --command CMD --manifest FILE` checks an adapter program on this machine with no daemon.

`update` installs the version next to the current one, switches the entry (or the workspace layer with `--workspace`), restarts the instance, and removes versions that nothing uses. `capabilities` starts an MCP server to list its tools, prompts, and resources, and keeps the result for 10 minutes. `gc` removes installed versions that no entry and no workspace layer uses.

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
