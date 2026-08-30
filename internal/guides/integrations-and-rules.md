---
topic: integrations-and-rules
title: Integrations, rules, events, and the queue
description: Create and connect integrations, test and fire rules, event triggers, and the dispatch queue.
---

# Integrations, rules, events, and the queue

An integration is a command that produces events. A job trigger of type `event` matches those events. See `trebi guide jobs-and-scheduling` for the `triggers[]` shape.

## Integrations

```bash
trebi integration list --json
trebi integration set slack --type stream --format json --cwd /opt/slack -- ./slack-stream --follow
trebi integration show slack
trebi integration test slack
trebi integration connect slack
trebi integration disconnect slack
trebi integration restart slack
trebi integration logs slack
trebi integration remove slack
```

`integration logs` (and `--follow`) blocks — **local only**; remote: poll `integration.logs`. `integration remove` is destructive — ask first.

## Cursors

Cursors track how far an integration has consumed.

```bash
trebi integration cursor show slack
trebi integration cursor set slack <last-event-id>
trebi integration cursor reset slack
```

## Rules

Test routing rules against a sample event before you put them in a job.

```bash
echo '{"channel":"ops","text":"deploy prod"}' | trebi rules test --match "e.channel === 'ops'" --extract "({t: e.text})" --json
trebi rules fire --integration slack --job my-job --workspace reports --match "e.channel === 'ops'" --event-file ./event.json
```

`--event-file` is **local only**; remote: pass `event` in input. `rules test` only evaluates. `rules fire` evaluates and enqueues on a match.

## Durable trigger events

```bash
trebi events list --limit 50 --json
trebi events retrigger <id>
```

`events retrigger` re-injects a stored event.

## Queue

The dispatch queue of pending launches.

```bash
trebi queue --status pending --json
trebi queue cancel <id>
trebi queue requeue <id>
```

`queue requeue` moves a blocked/done row back to pending.
