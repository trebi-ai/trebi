---
topic: runs-and-debugging
title: Inspect and recover runs
description: Show, timeline, transcript, log, retry/resume/replay, and approvals.
---

# Inspect and recover runs

A run is one launch of a job. `job run` returns when the session exists. To wait for the end, pass `--wait` (or use `runs wait`) instead of a poll loop. Do not kill a live run to "unstick" it.

## List and inspect

```bash
trebi runs list --json
trebi runs list --workspace staging --job report --json
trebi runs list --status running --job my-job --limit 50 --json
trebi runs list --harness claude --kind assistant --json
trebi runs list --since 24h --end-reason failed --json
trebi runs list --search deploy --json
trebi runs show <id> --json
trebi runs timeline <id> --json
trebi runs transcript <id> --json
trebi runs transcript <id> --raw --json
trebi runs stats --range mtd --json
trebi runs sessions --json
```

## Watch until it finishes

```bash
trebi job run my-job --wait 60000 --json
trebi runs wait <id> --wait-ms 60000
trebi runs show <id> --json
trebi bus --replay --topics run --limit 20 --json
trebi runs list --job my-job --limit 1 --json
```

`--wait` long-polls until the run ends, up to 120000 ms. The response is the final run row. A longer run needs a second call.

For the assistant, poll `trebi conversation messages <id> --json` the same way. See `trebi guide assistant`.

```bash
trebi runs attach <id>
```

`runs attach` is **local only**; remote: none — poll `runs transcript` / `runs logs`. Never attach from an agent session.

```bash
trebi runs message <id> "answer to the prompt"
```

## Recover

`resume` continues the same session. `replay` starts a fresh run from the same prompt. `retry` picks the right one from the failure reason — prefer it after a failure.

```bash
trebi runs resume <id>
trebi runs replay <id>
trebi runs retry <id>
trebi runs kill <id> [id...]
```

After `end_reason=session_gone` or `launch`, use `runs timeline` and `trebi service logs --level error --lines 100 --grep <id>`. `service logs` is **local only** in follow/`-f` form; remote: `system.logs`. Do not `sqlite3` / `find` under `$TREBI_HOME`. See `trebi guide observability`.

## Approvals

Approvals block a run until you decide. The daemon opens a pending row with the
question when the run starts to wait, and the decision closes the same row.

```bash
trebi runs approvals --state pending --json
trebi runs approvals --state decided --json
trebi runs approve <id> --decision allow
trebi runs approve <id> --decision deny
```

Inbox of pending approvals: `trebi guide inbox-and-alerts`.
