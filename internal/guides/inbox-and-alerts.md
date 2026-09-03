---
topic: inbox-and-alerts
title: Inbox and alerts
description: Drain the inbox, handle approvals, and read the alert feed.
---

# Inbox and alerts

The inbox is a queue you drain. Alerts are a read-only feed of every firing.

## Inbox

```bash
trebi inbox list --status unread --json
trebi inbox list --kind approval --json
trebi inbox show <id> --json
trebi inbox read <id>
trebi inbox read-all
trebi inbox resolve-all --kind approval
trebi inbox resolve <id>
```

Kinds: `approval` | `failure` | `health` | `watchdog`. `resolve` manually dismisses one item; `resolve-all` clears every open item, or one kind with `--kind`.

## Approvals (runs)

An approval inbox item points at a blocked run. Decide on the run:

```bash
trebi runs approvals --state pending --json
trebi runs approve <id> --decision allow
trebi runs approve <id> --decision deny
```

A pending row carries the question the agent asked. See `trebi guide runs-and-debugging`.

## Alerts

```bash
trebi alerts --since 2026-08-01T00:00:00Z --json
```

Alerts are a log, not a queue — reading them does not drain the inbox.

## Daily triage

```bash
trebi doctor --json
trebi inbox list --status unread --json
trebi runs approvals --json
trebi runs list --status failed --limit 20 --json
```

Health and logs: `trebi guide observability`.
