---
topic: connectors-and-rules
title: Connectors, rules, events, and the queue
description: Create and connect connectors, test and fire rules, routes for events, and the dispatch queue.
---

# Connectors, rules, events, and the queue

A connector is one external system. It can give **actions** to an agent run (an MCP server, or CLI commands plus a skill), **events** to the routes of the connection, and a **channel** for a conversation. The three blocks are optional and independent; a connector with no block is invalid. Routes decide which agent or job gets the events of a connection: see `trebi guide routing`.

A connector type is a package from the catalog, for example `whatsapp`. A **connection** is one account of a type that the user adds. A type can have many connections, for example a work phone and a home phone. Each connection has a stable id (`con_` plus a ULID), a **slug**, and a **title**. Jobs, agents, routes, and notify steps refer to the slug. The title is the name that the user sees. Each command that takes a connector accepts the slug or the id. The native connections `email` and `webhook` use the same word as id, slug, and type.

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

`connector set` sends a `PATCH`. `--title` sets the display name that the UI shows; an empty title restores the catalog title. `--slug` changes the slug. It also changes each reference to the old slug: the workspace layers, the `connectors:` lists of jobs and agents, the routes file of the connection (it moves to `routes/<new-slug>.yaml`; an existing file there fails the change with 409 `conflict`), and the notify `via:` steps. When one file cannot change, the slug stays and no file changes. `--input` merges into the stored inputs. `--patch-file` replaces the merge patch, `--manifest-file` replaces the inline manifest, and a file that holds `null` removes it. With `--manifest-file`, `set` creates a connection that does not exist. A slug matches `^[a-z0-9][a-z0-9-]{0,39}$` and does not start with `con_`; `email`, `webhook`, and `trebi` are reserved.

The events block has two protocols. `lines` (the default) reads one event per output line: `format` is `json` or `line`, `poll` runs the command again on an interval (at least `10s`), and `catchup.command` reads the events after `$TREBI_LAST_EVENT_ID` at start. Catch-up needs `format: json` and `id`, and it cannot be combined with `poll`. `trebi-connector/1` runs an adapter that speaks the connector protocol: it sends typed events, and it can log in to an account and hold a channel. A `channel` block needs `events.protocol: trebi-connector/1`.

A `lines` connector with `format: json` can map the message fields of each line. Each key is a dotted path into the JSON line; a numeric segment selects an array item:

```yaml
events:
  command: my-tool listen --json
  format: json
  id: id
  room: channel.id
  room_name: channel.name
  thread: thread_id
  sender: user.id
  sender_name: user.name
  type: kind
  text: text
```

The mapped fields work as the fields of a `trebi-connector/1` event: route `rooms` compares `room`, `contains` tests `text`, and the run gets the built-in vars (`text`, `sender`, `sender_id`, `room`, `room_name`, `thread`, `message_id`, `connection`). The `identity` path wins for the allowlist; without it, the `sender` value is the identity. The `sender` var is the `sender_name` value, else the `sender` value. A `thread` value starts a conversation, so `replies: continue` follows the thread; without a thread, each event starts its own conversation. A path that is not in the line leaves its field empty, and without `text` the event text is the whole line. When the manifest declares `types`, Trebi drops a line whose `type` value is not in the list and writes a `connector.parse_error` log line. Each key needs `format: json`.

A route `allowlist` checks only the value at the events `identity` path. A connector with no identity fails every allowlist. The `email` connector compares the sender address without case. Every other connector compares the value exactly.

A route `from` (`people`, `bots`, or `anyone`, the default) reads `sender.bot` of a `trebi-connector/1` event. A `lines` connector and an adapter that does not send `sender.bot` make every sender a person. A route `contains` tests the event `text`: what a person sees, with the readable parts of embeds and other rich content, one per line.

A job or agent receives connector actions through `connectors:` (or the agent `kit: connectors`). A job list adds to the kit list. The built-in name `email` needs no connection: it gives the run the native `trebi email send` command, the sender address in the prompt, and `TREBI_WORKSPACE` and `TREBI_AGENT` in the environment. A run with an MCP action needs a supported harness and runtime: claude, codex, and opencode on either runtime, and the ACP harnesses (copilot, cursor, gemini) on the CLI runtime. The CLI runtime asks the installed harness if it can take MCP servers; pi cannot. An unsupported pair fails the launch with `validation`, and doctor reports `kit_actions_unsupported` first. A resumed run keeps the actions of the run it resumes. A run that an event starts can also read the connection of that event when the list does not name it: it gets the operations with `readonly: true`, through `trebi connector call <slug> <operation>`, and no MCP, CLI, env, or secrets. A write operation on that connection answers 403. To give full access, add the slug to the job `connectors:` or the agent `kit.connectors`. `trebi job validate` reports `trigger_access_read_only` (info) or `trigger_access_none` (warning, no read-only operation) at `routes[<id>]`. Codex gets MCP secrets through the environment, never on the command line. For opencode, the injected servers merge into the `OPENCODE_CONFIG_CONTENT` of the job; an injected server wins on a name clash.

`connector logs` returns at once; `--follow` blocks — **local only**; remote: poll `connector.logs`. Human output starts each line with the local time `YYYY-MM-DD HH:mm:ss`; `--json` prints one raw row per line. `connector delete` is destructive — ask first.

## Scope

A connection has a `scope`: the workspaces that see it. `global` (the default when the field is missing) means every workspace. A list of workspaces means only those workspaces. The connection keeps one process and one login; the scope changes only who sees it. A workspace outside the scope cannot use the connection: a job or agent that names it in `connectors:` fails to launch, a route does not fire, a notify step on it is skipped with a warning, an operation call and a live source are refused, and a room bind answers `conflict`. A list row of a workspace has `in_scope` and `scope` (`["global"]` or workspace ids). A caller with no workspace sees every connection.

`connector add` of a connector with an account (a secret input or a login) sets the scope to the current workspace. Any other connector stays global. `--scope` on `add` and `apply` sets the scope: `global`, or workspace names separated by commas. The daemon stores workspace ids; the CLI and the API accept names or ids.

```bash
trebi connector scope whatsapp-work               # show
trebi connector scope whatsapp-work ocral,btd     # only these workspaces
trebi connector scope whatsapp-work global        # every workspace
trebi connector add whatsapp --slug wa-team --scope ocral
```

Doctor reports `connector_out_of_scope` for a kit connector, a job connector, a route, or a notify step that names a connection outside its scope. It reports `trigger_connector_unknown` and `trigger_connector_disabled` for a route on a connector that does not exist or that the workspace turned off. It reports `route_connection_unknown` for a routes file with no connection. It reports `trigger_access_none` for a route on a connection that is not in the job `connectors:` or the agent kit and has no read-only operation.

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

A connector can log in to an account. A `trebi-connector/1` adapter can also list the rooms of its channel (chats, groups, mailboxes).

```bash
trebi connector login-methods slack --json
trebi connector login whatsapp
trebi connector login slack --method token
trebi connector logout whatsapp
trebi connector rooms whatsapp --query ops --json
trebi connector threads whatsapp <room> --json
trebi connector senders whatsapp --query ana --json
trebi connector dependents whatsapp --json
trebi connector bind whatsapp <room> --workspace reports --operator assistant
```

A connector has one or more login methods. `connector login-methods` lists them in this order: `existing` (use a login that is already on this computer), `browser`, `phone`, and `key` (paste a key). An unavailable method has a `reason`. `connector login` uses the first available method, and `--method <id>` picks another. It prints each step (a code, a QR code, a link, or a list of choices) and waits for the user. You never collect a key in the chat: for a `key` method, the command prints the link of the login form in Trebi, and you give that link to the user. Ask the user before you log out. `connector senders` lists the recent senders from the stored events; put an `identity` in the `allowlist` of a route, and a room `id` in its `rooms`. `connector dependents` lists the routes on the connector (disabled and inactive ones too) and the rooms that are bound to the assistant or an agent. `connector threads` answers 400 `unsupported` when the connector has no `threads` feature. `connector bind` sends the messages of a room to the assistant or to `agents/<name>` of a workspace; `--thread` binds one thread only. A notify route step names the connection slug and a room id: `trebi config set notify.route '[{"via":"whatsapp","to":"<room>"},{"via":"email"}]'`.

## Watch things

A connector with `events.subscriptions` in its manifest can watch things on its platform, such as a repository, a team, or a list. `connector show <slug> --json` gives the form: `events.subscriptions.fields`.

```bash
trebi connector watch-options github repo --query trebi --json
trebi connector watch github --set repo=trebi-ai/trebi --events pull_request,issues
trebi connector watch linear --set team=ENG --set resources=Issue --set resources=Comment
trebi connector watching github --json
trebi connector watch-set github <id> --events pull_request
trebi connector watch-finish notion <id> --set signing_secret=<value>
trebi connector unwatch github <id>
```

A field with `dynamic: true` takes a `value` from `watch-options`. Repeat `--set` for a field with `multiple: true`. Leave `--events` out to keep every event type. `watching` gives one row for each watch with its `mode` and `state`: `active` is live, `polling` checks every few minutes, `action_required` needs a person, and `error` has a `message`. For `action_required`, tell the user the action text and the values to copy, then run `watch-finish` with the values that the user gives. Live updates need a linked Trebi Cloud account; without it, a connector that can poll still polls. `watching --sync` asks the connector for a new state now.

## Choose the events

A connection sends all of its events to Trebi by default. The user can keep some types, or none. With none, the connection stays connected for its actions.

```bash
trebi connector events whatsapp
trebi connector events whatsapp only message,reaction
trebi connector events whatsapp only message --other
trebi connector events whatsapp none
trebi connector events whatsapp all
```

With no mode, the command shows each type with the kept and skipped counts of the last hour. `--other` also keeps the types that the connector does not list. Trebi does not store a skipped event and starts no job from it. A change back to `all` does not bring back the skipped events. A change does not restart the connection. `connector test` refuses a skipped type. `doctor` reports `connector_event_skipped` when a job starts on a type that its connection skips. Ask the user before you skip a type that a job uses. MCP: `connector set` with `receive: {mode, types, other}`.

## Conversations

A job route with `replies: continue` keeps each conversation of a connector in one run session. The daemon finds the conversation root of each `trebi-connector/1` event: the thread, else the message that the event quotes or answers, else the whole DM. A channel that does not declare the `threads` feature is threadless: its lane is the room. Per platform:

- Discord and Slack: the thread. A thread id is the id of its start message, so the start message and its replies are one conversation.
- Telegram: the topic, or the reply chain.
- WhatsApp has no threads: a DM is one conversation. In a group, `replies: continue` changes nothing. The lane is the group, and each message must pass the route. A message that passes joins the live run of the group.
- Email: the thread root.

A job route with `chat: reply` and `rooms` answers in the chat of those rooms as its owner agent. The owner agent stops answering there, and `connector dependents` still lists both. See `trebi guide routing`.

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

## Add an online MCP server

An online MCP server runs on the internet, for example `https://mcp.granola.ai/mcp`. Trebi starts no program for it. Many such servers need a login in the browser (OAuth).

```bash
trebi connector check-link https://mcp.granola.ai/mcp
trebi connector add https://mcp.granola.ai/mcp --title Granola
trebi connector login granola
trebi connector oauth show granola --json
trebi connector oauth set granola --mode header
```

`check-link` with an `https` address that is not a GitHub link asks the server and prints `runs: online` and `login: needed` when the server asks for a login. `connector add <address>` writes the connector and prints the login command. `connector login` opens the browser on this computer and finishes when the browser comes back. When the browser cannot come back, paste the address of the last page. `--no-browser` prints the link only. Ask the user to do the login; you never see the token. Over MCP, `connector.create` takes `url` and `login: true`, and `connector.login_status` with `flow_id` waits for the end of the login.

A run gets the server through Trebi (mode `proxy`, the default), so the run never holds the token. Trebi renews the token, applies the tool states (`connector operation <slug> <tool> on|ask|off`), and turns a question of the server into an ask on the run. Mode `header` gives the run the token itself; use it only for a harness that cannot reach Trebi, because the run cannot renew the token. `oauth set` also takes `--client-id` with `--client-secret-name <secret>` for an own app, and `--scope` for more access. When the login ends or the server needs more access, an Inbox alert asks the user to log in again, and a run that needs the connector fails before launch with the reason `connector_login`. `connector logout` revokes the tokens.

## Add a connector from a GitHub link

A folder in a GitHub repository can be a connector when it holds a `trebi-connector.yaml`. Trebi did not review this code, and it runs on this computer. Ask the user before you install a link.

These link forms work:

- `https://github.com/<owner>/<repo>`: the root of the default branch.
- `https://github.com/<owner>/<repo>/tree/<ref>/<path>`: one folder at a branch, a tag, or a commit. `github.com/...` with no scheme also works.
- `<owner>/<repo>[/<path>][@<ref>]`: the short form. A registry name (for example `ai.example/server`) is not a link, because a GitHub owner has no dot.

```bash
trebi connector check-link https://github.com/flarco/cli-tools/tree/main/vitally-cli
trebi connector check-link flarco/cli-tools/vitally-cli --json
trebi connector add flarco/cli-tools/vitally-cli --check-only
trebi connector add flarco/cli-tools/vitally-cli --slug vitally --input VITALLY_API_KEY=secret:vitally_key
trebi connector add flarco/cli-tools/vitally-cli@3f2a9c1e0b7d4a6f8e5c2b1a0d9f8e7c6b5a4d3c --yes
```

`check-link` downloads the folder at one commit and checks it. It installs nothing and starts no program. It prints the connector name, the version, the commit, and one row for each check. The exit code is 1 when the link cannot install. The groups of the rows are:

- `manifest`: the folder has a `trebi-connector.yaml`. When it has none, `found` lists the sub-folders that have one.
- `valid`: the manifest decodes and passes the rules. Each unknown key or bad field gets its own row with its `field`.
- `program`: the install method applies. A link needs `install.build`, `install.github_release`, or no program. A catalog-only method (for example `brew`) fails.
- `inputs`: each required setting has a label.
- `skill` and `events`: the skill files and the events block of the folder.

A row is `pass`, `warn`, `fail`, or `skip`. Any `fail` blocks the install. With `--strict`, a `warn` row also fails the command. `connector add <link>` runs the same check, prints the report, and asks "Trebi did not review this connector. It runs on this computer. Install? [y/N]". `--yes` skips the question. Then the daemon builds or downloads the program, runs the conformance suites in a sandbox, and creates the connection with `from: github`. A failed suite blocks the install; `--force` installs anyway (advanced; only when the user asks). The bus topic `connector.install.step` shows the progress.

A private repository works when the GitHub login of this computer (`gh auth login`) can read it. When the login cannot read it, the check returns `not_found`. Trebi keeps the token in memory only; it never writes it to disk.

A link with a 40-character commit is pinned: it never changes. A link with a branch (or no ref, which is the default branch) follows the branch. The daily update check compares the branch head with the commit of the connection. A new commit shows as `update: {version, commit}` on the row and as an Inbox item. `connector update <slug>` installs the new commit after the same check and the same suites. `connector versions` does not apply to a link. Each commit gets its own package folder `<version>+g<commit12>`, and `connector gc` removes the ones that nothing uses. When the files of a commit change on GitHub, Trebi refuses the new download, and `trebi doctor` reports `connector_digest_mismatch`.

## Make a folder a connector

Add a `trebi-connector.yaml` to the folder of the tool. This example makes a Go CLI tool a connector. The program builds from the folder with `go build`, and the skill is the `SKILL.md` in the same folder:

```yaml
schema: trebi-connector/1
name: vitally
title: Vitally
description: Accounts, users, notes, and tasks of one Vitally workspace.
version: 0.1.0
publisher: community
install:
  build: {go: .}
  bin: vitally-cli
  version_command: vitally-cli --version
setup:
  inputs:
    - {name: VITALLY_API_KEY, label: API key, secret: true, required: true}
  login:
    - {id: key, type: key, input: VITALLY_API_KEY, test: vitally-cli accounts list --limit 1}
skill: {path: .}
actions:
  cli: {commands: [vitally-cli]}
```

- `install.build.go` is the package to build, relative to the folder. The folder (or a parent in the download) needs a `go.mod`. The user needs Go on this computer.
- `install.bin` is the program name. `version_command` must print the manifest `version`.
- Each `setup.inputs` entry is one setting that the user fills. Give a `label`, and set `secret: true` for a key or a token.
- Each `setup.login` entry is one way to log in. `type` is `existing`, `browser`, `phone`, or `key`. A `key` method with `input` names a secret input: Trebi asks for the key on the Login tab and saves it. The optional `test` command runs with the new key, and a failure shows its last lines to the user. A method with no `input` is for a `trebi-connector/1` program, which answers `auth/methods` and runs the login itself.
- `skill.path` is the folder of the `SKILL.md`; `.` is the connector folder.
- `actions.cli.commands` lists the commands that a run can call.

Commit and push the change, then run `trebi connector check-link <link>` until it passes.

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

`connector conformance <slug>` checks a running protocol connector against its manifest; `--room` adds a real test send, and `--suite` limits the suites; `--strict` makes a `warn` row fail the command. `connector conformance --command CMD --manifest FILE` checks an adapter program on this machine with no daemon.

`update` installs the version next to the current one, switches the connection, restarts its process, and removes versions that nothing uses. `capabilities` starts an MCP server to list its tools, prompts, and resources, and keeps the result for 10 minutes. `gc` removes installed versions that no connection uses and no holder keeps.

A program outside Trebi, such as perception, can run a connector itself. Trebi then installs only the package: no connection and no process. The program keeps its packages with `package keep`, so `gc` does not remove them. A package version is the folder name: the catalog version, or `<version>+g<commit12>` for a link.

```bash
trebi connector package install whatsapp --version 1.2.0
trebi connector package install https://github.com/acme/tools/tree/main/kvm   # asks first, as add does
trebi connector package list
trebi connector package keep perception whatsapp@1.2.0 kvm@0.1.0+g0123456789ab
```

The commands of the installed connectors are also in `~/.trebi/bin/connectors/`, one link each, and the links follow each update. Jobs and connectors do not need it. To call a connector program from a shell, add the folder to `PATH` once, for example `export PATH="$HOME/.trebi/bin/connectors:$PATH"` in `~/.zshrc`. Doctor reports `connector_command_clash` when two connectors have one command name.

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
trebi rules fire --connector slack --job ops/my-job --workspace reports --match "e.channel === 'ops'" --event-file ./event.json
```

`--event-file` is **local only**; remote: pass `event` in input. `rules test` only evaluates. `rules fire` evaluates and enqueues on a match.

Test the filters of a route against stored events. The checks run in route order: `--room` (the rooms the route takes), `--allowlist` (the senders), `--from` (`people`, `bots`, or `anyone`), `--contains` (repeatable; the text must hold one entry), extract, then match. `--match` is optional. The result names the `stage` that stopped the event: `room`, `sender`, `from`, `contains`, `extract`, or `match`. On a match, `vars` holds the built-in vars of the event plus the extract vars. `--replies continue` sets `conversation_key` to the key of a job with `replies: continue`. Add `--job` (and `--workspace`) with `--replies continue` to test as that job: an event in an open conversation of the job gets `follow_up` true and skips `from`, `contains`, and match. A bot message in an open conversation stops at `from`. Without `--job`, the test treats each event as the first message of its conversation.

```bash
trebi rules test --event-id <event-id> --room 120363430107848596@g.us --match "event.type === 'message'" --json
trebi rules test --event-id <event-id> --from bots --contains failure --replies continue --json
trebi rules test-batch <event-id> <event-id> --allowlist 15551234567@s.whatsapp.net --json
```

`rules test-batch` takes at most 50 events.

The checks also include `types` (first) and `inputs` (last). With `--job`, a match fills the inputs of the first route of the job (`--route <id>` picks another) and gives `inputs` and `missing`. A `gets: by_hand` route records the outcome `manual` and starts no run; `trebi event run <event-id> --job <job> --route <id>` starts it. To test a whole connection, use `trebi connector route-test` (`trebi guide routing`). See `trebi guide jobs-and-scheduling`, "Inputs and hand runs".

## Hosted webhooks

A hosted webhook is a public URL the cloud receives and pushes over the link socket. Mint one per sender. The first path segment of the posted path becomes the event `label`, so one hook fans out to many jobs by path.

```bash
trebi hooks create --workspace reports --label deploy
trebi hooks list
trebi hooks delete <id>
```

`hooks create` prints the URL once. Handle it like a webhook secret: anyone holding it can post an event. `hooks delete` is destructive — ask first. `hooks list` masks the URLs; `--reveal` prints them and writes an audit entry.

A route in `routes/webhook.yaml` sends a delivery to a job. `event.headers` carries the sender signature allowlist, so `match` can verify the sender:

```yaml
routes:
  - job: ops/deploy
    gets: every_message
    match: event.path === "/deploy" && event.headers["x-github-event"] === "push"
```

`extract` pulls values out of the payload. It runs before `match` and rebinds `event` to its result, so a rule that extracts reads the extracted fields in `match`:

```yaml
routes:
  - job: ops/deploy
    gets: every_message
    extract: "({ ref: json(event.body).ref })"
    match: event.ref === "refs/heads/main"
```

The payload is `hook_id`, `workspace_id`, `label`, `method`, `path`, `content_type`, `headers`, `body`, and `received_at`. A hook reaches only the routes of its own workspace. `body` stays a string: call `json(event.body)` for a value. Delivery is at-least-once; a duplicate delivery id never fires twice.

## Durable events

```bash
trebi event list --limit 50 --json
trebi event list --after <event-id> --connector <slug> --json
trebi event list --after <event-id> --follow --json
trebi event retrigger <id>
```

`event list` shows the newest events first, and `--cursor` pages back. `--after <event-id>` reads forward: it gives the events after that id, oldest first, one row per event in the `event show` shape, and `next_after` names the id for the next read. Event ids are ULIDs, so id order is time order. `--follow` keeps the read open and prints each new event as one JSON line; it blocks, so do not use it from an agent session. To continue after a stop, give the last printed id as `--after`; you get no gap and no duplicate. Without `--after`, `--follow` starts at the oldest event. `--connector`, `--workspace`, `--since`, and `--search` filter both forms. The API form is `GET /v1/events?after=<id>` and `GET /v1/events?after=<id>&follow=1` (NDJSON; a line with no `id` is a keepalive).

`event retrigger` matches a stored event against the current routes again. It writes no new event row and works for every connector, `email` and `webhook` included.

## Queue

The dispatch queue of pending launches.

```bash
trebi queue --status pending --json
trebi queue cancel <id> [id...]
trebi queue requeue <id>
```

`queue requeue` moves a blocked/done row back to pending.
