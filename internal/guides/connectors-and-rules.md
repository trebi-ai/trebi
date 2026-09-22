---
topic: connectors-and-rules
title: Connectors, rules, events, and the queue
description: Create and connect connectors, test and fire rules, event triggers, and the dispatch queue.
---

# Connectors, rules, events, and the queue

A connector is a command that produces events. A job trigger of type `event` matches those events. See `trebi guide jobs-and-scheduling` for the `triggers[]` shape.

`trebi connector list` also shows **inherited** MCP servers discovered from the user harness configs the bridge injects (`~/.claude.json` / `~/.claude/mcp.json`, `~/.codex/mcp.json`, `~/.cursor/mcp.json`, `~/.pi/agent/mcp.json`). Those rows are read-only: connect, edit, and delete are refused. Edit the origin file instead. Managed rows are the ones in `connectors.yaml`.

## Connectors

```bash
trebi connector list --json
trebi connector set slack --type stream --format json --cwd /opt/slack -- ./slack-stream --follow
trebi connector show slack
trebi connector test slack
trebi connector connect slack
trebi connector disconnect slack
trebi connector restart slack
trebi connector logs slack
trebi connector remove slack
```

`connector logs` (and `--follow`) blocks — **local only**; remote: poll `connector.logs`. `connector remove` is destructive — ask first.

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
