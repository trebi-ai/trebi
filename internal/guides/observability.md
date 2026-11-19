---
topic: observability
title: Bus, logs, doctor, and stats
description: How to pull logs, follow the event bus, and read doctor findings.
---

# Bus, logs, doctor, and stats

## Doctor and health

```bash
trebi status --json
trebi doctor --json
trebi service status --json
```

`status` is the one-call operator view: version, doctor counts, harness auth, workspace count, running and waiting runs, unread inbox, and the next due schedule. Start there. When the daemon is down it falls back to the OS service status.

`service status` is **local only**; remote: `system.health`. If `doctor` is healthy, stop. Do not "fix" the database, edit `$TREBI_HOME`, or send signals to trebid.

```bash
trebi doctor --fix
trebi doctor fix <id>
```

## Event bus

`trebi bus` is the live stream. It is not the trigger pipeline (`trebi event`) and it is not a complete audit trail — retention is 7 days.

```bash
trebi bus --replay --topics run --limit 10 --json
trebi bus --topics run.failed,alert --limit 20 --json
trebi bus --topics run --replay --limit 100 --json
trebi bus --cursor <seq> --limit 20 --json
```

`--topics` is server-side (family or exact topic). `--entity` is a client-side filter on the same frames. Prefer `--topics run` for "run events".

Frames are thin: `entity`, `id`, `action`, `revision`. They carry no payload — refetch with `trebi runs show` / `trebi job show` when you need the object.

```bash
trebi bus
```

`trebi bus` without `--limit` follows live and blocks — **local only**; remote: `system.bus_poll` with `cursor` / `topics`. `--limit` dumps matching history (up to N frames) and exits. Never omit `--limit` from an agent session.

```bash
trebi bus --entity run --id <id> --action updated --limit 20 --json
```

`--entity` is **local only** (client-side filter); remote: pass `topics` and filter the returned events.

Trigger events (durable): `trebi guide connectors-and-rules`.

## Event feed

The bus frame `event.created` carries only the connector and the connection, and the bus resets when a cursor is older than its retention. To read every trigger event in order, read the event log instead:

```bash
trebi event list --after <event-id> --json
trebi event list --after <event-id> --follow --json
```

`--after` gives the events after that id, oldest first, with the payload of `event show`. `--follow` prints each new event as one JSON line and blocks — **local only**; remote: `event.list` with `after`, then call it again with `next_after`. Keep the last id you read; give it as `--after` to continue with no gap and no duplicate.

## Service logs

```bash
trebi service logs --lines 200 --level error --grep timeout
```

`service logs` takes `--lines` / `--grep`, not `--limit`. Drop follow/`-f` unless you intend to block (`-f` is **local only**; remote: `system.logs`).

The daemon logs each API request as `api.request`. Successful reads of `/v1/system/health` and `/v1/runs/session-state` log at debug (the tray polls them). Everything else, and any poll that fails, logs at info.

## Stats and drain

```bash
trebi runs stats --json
trebi runs stats --range mtd --json
trebi runs stats --range 7d --job ops/my-job --json
trebi runs activity --range today --json
trebi runs activity --range 7d --agent ops --json
trebi runs activity --range 30d --workspace staging
trebi runs sessions --json
trebi service drain
trebi service undrain
```

`runs stats` answers "how much did we spend". It returns `totals` for the range,
`mtd` for the month to date, and one `days` row per day. The range is `24h`,
`7d`, `30d` (default), or `mtd`.

`runs activity` answers "what did my agents do". It returns one row per agent with run counts, tokens, cost, and planned fires, per time bin. A run counts in the bin of its start. The range is `today` (15 minute bins), `7d` (default, 1 hour bins), or `30d` (4 hour bins). The window ends at the next local midnight.

Drain returns 503 `draining` to automatic producers. Manual launches still pass. Use drain before maintenance, then `undrain`.

```bash
trebi service shutdown
```

`service shutdown` asks the daemon to exit. `service start` / `stop` / `restart` / `install` / `uninstall` / `migrate-db` / `daemon` are **local only**; remote: none (if MCP is down, there is no daemon to talk to). Never `kill` the daemon PID.

## Cloud link

```bash
trebi cloud status --json
trebi cloud link --code ABCD-EFGH --name fritz-mbp
trebi cloud unlink --force
trebi cloud addresses --reveal
```

The account page mints the code under "Link a device"; it expires in 10 minutes. `cloud link` prints the plan and the granted features. `cloud unlink` is destructive: remote clients lose access. It does not revoke the device in the cloud; the account page revokes.

A linked daemon caches `plan` and `features` and serves them to the UI. The cloud is the source of truth, so the cache decides what the screen shows and never what the user gets. `degraded` in `cloud status` means the socket is down. `unauthorized` means the cloud revoked the device and the daemon stopped redialing.
