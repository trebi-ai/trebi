---
topic: connectors-and-rules
title: Connectors, rules, events, and the queue
description: Create and connect connectors, test and fire rules, event triggers, and the dispatch queue.
---

# Connectors, rules, events, and the queue

A connector is a command that produces events. A job trigger of type `event` matches those events. See `trebi guide jobs-and-scheduling` for the `triggers[]` shape.

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
