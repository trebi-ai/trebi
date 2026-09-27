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
trebi connector list --capability events --json
trebi connector set slack --events-command 'slack-stream --follow' --format json --cwd /opt/slack --restart on-failure
trebi connector set slack --poll 15m --events-command 'slack-fetch --since $TREBI_LAST_EVENT_ID' --catchup-command 'slack-fetch --since $TREBI_LAST_EVENT_ID' --id ts --identity user
trebi connector set notion --mcp-command npx --mcp-arg=-y --mcp-arg @notionhq/notion-mcp-server
trebi connector set mail --channel-ref '{{ chat_id }}' --send send-mail.sh --typing typing-mail.sh --ack ack-mail.sh
trebi connector show slack
trebi connector test slack
trebi connector connect slack
trebi connector disconnect slack
trebi connector restart slack
trebi connector logs slack
trebi connector remove slack
```

`trebi connector set <name>` sends a `PATCH`: a flag replaces that part of the definition and the stored blocks stay. `trebi connector set slack --no-events` removes the events block. `trebi connector apply --file ./slack.yaml` writes a whole definition with `PUT`, for an import.

A job or agent receives connector actions through `connectors:` (or the agent `kit: connectors`). A run with an MCP action needs a supported harness and runtime: claude, codex, and opencode on either runtime, and the ACP harnesses (copilot, cursor, gemini) on the CLI runtime. An unsupported pair fails the launch with `validation`.

`connector logs` (and `--follow`) blocks — **local only**; remote: poll `connector.logs`. `connector remove` is destructive — ask first.

## Registry

Install an MCP server from a registry. `search` reads every registry in `connectors.registries`; the default holds the official MCP Registry only.

```bash
trebi connector search postgres --registry official --limit 20
trebi connector install io.github.modelcontextprotocol/server-github --as github --env GITHUB_PERSONAL_ACCESS_TOKEN=secret:github_pat
trebi connector install acme/weather --package 0 --env WEATHER_API_KEY=secret:weather
trebi connector install acme/weather --remote 0
```

`install` writes the variables the entry marks as secret, then creates the connector with a `source` block. `--package` and `--remote` select one entry of the server (default: the first package, else the first remote that speaks `streamable-http` or `sse`). An entry that contributes a skill writes it under `~/.trebi/skills/<name>/` with provenance `connector`; `connector remove` deletes it again. An install runs third-party code on this machine, so ask the user before you install.

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

`events retrigger` re-injects a stored event.

## Queue

The dispatch queue of pending launches.

```bash
trebi queue --status pending --json
trebi queue cancel <id> [id...]
trebi queue requeue <id>
```

`queue requeue` moves a blocked/done row back to pending.
