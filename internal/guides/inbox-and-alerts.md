---
topic: inbox-and-alerts
title: Inbox and alerts
description: Drain the inbox, handle approvals, read the alert feed, set the alert route, and retry failed sends.
---

# Inbox and alerts

The inbox is a queue you drain. Alerts are a read-only feed of every firing.

## Inbox

```bash
trebi inbox ls --unread --json
trebi inbox ls --kind approval --json
trebi inbox ls --done --json
trebi inbox show <id> --json
trebi inbox read <id>...
trebi inbox unread <id>...
trebi inbox archive <id>...
trebi inbox archive --all --kind health
trebi inbox unarchive <id>...
```

Kinds: `approval` | `request` | `failure` | `health` | `watchdog` | `escalation`. An item is open (in the inbox) or Done. Read state is separate from Done state. `archive` moves items to Done; `unarchive` moves them back. `--all` acts on every open item, or one kind with `--kind`. Trebi closes an item when its cause goes away (`done_by: system`). An archive of an open approval or request needs `--yes`: the run still waits after the archive. `needs_action` marks an open approval, an open request, and an open blocker or critical alert. Trebi deletes a Done item after `inbox.done_retention` (default 720h).

## Approvals (runs)

An approval inbox item points at a blocked run. Decide on the run:

```bash
trebi runs approvals --state pending --json
trebi runs approve <id> --decision allow
trebi runs approve <id> --decision deny
```

A pending row carries the question the agent asked. See `trebi guide runs-and-debugging`.

## New skills (agents and jobs)

When an agent or job run creates a skill, an approval item of alert kind `skill_created` opens. The body holds the note of the run and the scope. The actions are "Change scope…", "Open", and "Approve" (only when the skill is pending). A pending skill does not reach runs until a person approves it:

```bash
trebi skill approve <name> --scope agent --owner <agent> --workspace <ws>
trebi skill move <name> --scope agent --owner <agent> --workspace <ws> --to-scope workspace --to-owner <ws>
```

The item closes when the skill is approved, moved, or deleted. See `trebi guide skills-secrets-config`.

## Requests (jobs)

A request inbox item points at a request that a job run opened. `request_id` on the item names it. Answer the request, not the item: Trebi closes the item when the request closes.

```bash
trebi inbox ls --kind request --json
trebi request show <request_id>
trebi request answer <request_id> approve
trebi request dismiss <request_id>
```

An email reply to the request mail also answers a request with one form: `approve` or `reject` on the first line, or one option for a `choose` form. Any other reply goes to the job as a message. A request with more forms takes no answer by mail. Open the link in the mail to answer it. See `trebi guide requests`.

## Held mail (email replies)

An agent that sends mail can get a reply. The reply reaches the agent when the sender is already on the thread and the safety check passes. A reply from a new sender, a reply that the check holds, and a reply whose check did not finish wait in the inbox as an `email_held` item. The agent does not read a held reply.

```bash
trebi email held --json
trebi decisions show <decision_id>
trebi email release <id> --scope once      # this mail only
trebi email release <id> --scope thread    # also trust the sender in this thread
trebi email release <id> --scope sender    # also add the address to email.accept of the agent
trebi email release <id> --scope domain    # also add @domain to email.accept
trebi email ignore <id>
```

Only the user releases or ignores held mail. When the check found a risk, the release needs `--yes`: tell the user the reasons first. A release records `disagree` on the decision, and an ignore records `agree`. `trebi decisions stats` shows how often the check agrees with the user.

## Alerts

```bash
trebi alerts --since 2026-08-01T00:00:00Z --json
```

Alerts are a log, not a queue — reading them does not drain the inbox.

## Alert routes

An alert goes along a route: an ordered list of steps `{via, to}`. `via` is `email` or a connector with a `channel` block. The first step that sends stops the route. A failed step moves the alert to the next step. When no step is left, the item stays in the inbox with `delivery: failed` and a `view_outbox` action. `inbox show` lists the `deliveries` of the item.

The route comes from the first layer that sets `notify`: the job, the owner agent, the workspace, then the global config. With no layer set, the route mails the contact. `notify: []` sends nothing. The contact follows the same order; the global contact is `email.admins`.

```bash
trebi notify route --workspace <ws> --job <job> --json
trebi notify test --workspace <ws>
trebi notify test --workspace <ws> --step 1
trebi config set notify.route '[{"via":"whatsapp","to":"+15551234567"},{"via":"email"}]'
trebi workspace set <ws> --notify '[{"via":"email","to":"admins"}]' --contact '["ops@acme.com"]'
trebi job patch <job> 'notify=[]' --workspace <ws>
```

For email, `to` is `contact` (the default), `admins`, or one address. For a connector, `to` is the channel ref. A step with a `problem` (`email_off`, `no_contact`, `unknown_connector`, `no_channel`) is skipped, and doctor reports `notify_route_invalid`. `notify test` sends one message per usable step, with one attempt each, and does not stop at a failure.

## Outbox

Every outgoing message is an outbox row: a channel reply, an email, or an alert mail. A failed attempt keeps the row `pending` with its `error` and `next_attempt_at`. After the last attempt the row is `failed`, and a `send_failed` failure item opens in the inbox.

```bash
trebi outbox ls --status failed --json
trebi outbox ls --kind email --workspace <ws> --json
trebi outbox show <id> --json
trebi outbox retry <id>
trebi outbox cancel <id>
```

`retry` takes a `failed` or `canceled` row. `cancel` takes a `pending` or `failed` row. A sent row resolves its `send_failed` item.

## Daily triage

```bash
trebi doctor --json
trebi inbox ls --unread --json
trebi runs approvals --json
trebi runs list --status failed --limit 20 --json
```

Health and logs: `trebi guide observability`.
