---
topic: runs-and-debugging
title: Inspect and recover runs
description: Show, turns, timeline, transcript, log, retry/resume/replay, and approvals.
---

# Inspect and recover runs

A run is one harness session of a job. Resume reopens the same run as a new turn: the run id stays, and `turn_count` goes up. A run between turns shows the status of its last turn. `duration_ms` and the cost sum all turns. A rerun or a fresh session is a new run, and its `previous_run_id` names the run it follows. A run that waits for a person is `parked`; it resumes with the answer (`trebi guide requests`). A daemon restart pauses a live run: it is `parked` with `end_reason=restart`, and it resumes by itself as its next turn when the daemon starts again. When the harness cannot resume, the run ends `failed` with `session_gone`; Trebi never starts it again from the start.

`job run` returns when the session exists. To wait for the end, pass `--wait` (or use `runs wait`) instead of a poll loop. To see progress while it runs, pass `--follow` (or use `runs watch`). Do not kill a live run to "unstick" it.

## List and inspect

```bash
trebi runs list --json
trebi runs list --workspace staging --job report --json
trebi runs list --status running --job agents/ops/my-job --limit 50 --json
trebi runs list --harness claude --kind agent --json
trebi runs list --since 24h --end-reason failed --json
trebi runs list --search deploy --json
trebi runs show <id> --json
trebi runs turns <id> --json
trebi runs timeline <id> --json
trebi runs transcript <id> --json
trebi runs transcript <id> --raw --json
trebi runs stats --range mtd --json
trebi runs sessions --json
```

## Watch until it finishes

```bash
trebi job run agents/ops/my-job --wait --json
trebi job run agents/ops/my-job --wait --timeout 9m --json
trebi job run agents/ops/my-job --follow
trebi runs wait <id> --timeout 9m
trebi runs watch <id>
trebi runs watch agents/ops/my-job
trebi runs watch agents/ops/my-job --next
trebi runs show <id> --json
trebi bus --replay --topics run --limit 20 --json
```

`--wait` and `runs wait` block until the run ends and print the final run row. They have no time limit; `--timeout` sets one. `--follow` and `runs watch` print the events of the run (assistant text, tool calls, state changes) and then the result line. `runs watch <job>` follows the live run of the job, else its latest run. `--next` waits for the next run to start, for example the next scheduled run. Ctrl-C stops the watch; the run continues. A launch that goes to the queue prints the `runs watch <job> --next` command to use.

All of them exit with the same codes: 0 complete or skipped, 1 failed, exited, or timed out, 2 parked (a request waits for a person, or a restart paused the run), 3 still running when the timeout came. A human outcome from `runs mark` wins over the status. With `--json`, `runs watch` prints one frame per line and the final run row last.

From an agent session, keep each call below the timeout of your shell tool: use `--timeout 9m`, and call `runs wait <id> --timeout 9m` again while the exit code is 3.

The assistant has no runs. Use `trebi assistant sessions` to find its conversations, then poll `trebi conversation messages <id> --json`. See `trebi guide assistant`.

```bash
trebi runs attach <id>
```

A run's runtime is `cli` by default: the harness binary runs as a subprocess and the daemon speaks its structured protocol. `attach` on a CLI run streams normalized events (user, assistant, tool, permission, result, status); `runs logs -f` always works and is the remote-friendly view. `runtime: tui` in the job document, `tools.<harness>.runtime tui` in config, or the incident-only `harness.runtime: tui` puts that run back in a terminal pane, where `attach` is the pane. A CLI run that fails closed (protocol drift, missing login, quota) leaves a doctor hint; relaunch or resume with `runtime: tui` when needed — there is no silent TUI fallback. Transcripts prefer the live wire events, then the vendor session file. Each transcript entry has `run_turn`, the turn that wrote it.

`runs attach` is **local only**; remote: none — poll `runs transcript` / `runs logs`. Never attach from an agent session.

```bash
trebi runs message <id> "answer to the prompt"
trebi runs message <id> "what is wrong here?" --attach shot.png   # send an image; repeat --attach for more
```

`--attach` uploads a PNG, JPEG, GIF, or WebP file to the run workspace and sends its id with the message. An image with no text is allowed. Pi and pane runs get the image as a file path line.

## Recover

`resume` continues the same session. `replay` starts a fresh run from the same prompt. `retry` picks the right one from the failure reason — prefer it after a failure. `mark` overrides the outcome of a finished run without launching anything; the machine status stays in stats, while job health and the inbox read the outcome.

```bash
trebi runs resume <id>
trebi runs replay <id>
trebi runs retry <id>
trebi runs mark <id> success --note "digest was sent"
trebi runs mark <id> failed
trebi runs mark <id> --clear
trebi runs kill <id> [id...]
```

A job run that cannot do its task must say so. Stop, do not work around the block, and end the last message with the failure line from the prompt: `TREBI_RUN_FAILED_<key>: <reason in one line>`. Trebi then ends the run `failed` with `end_reason=failed`, and the "Run failed" alert shows the reason. A run that ends with no failure line is `complete`.

After `end_reason=session_gone` or `launch`, use `runs turns` to find the turn that failed, then `runs timeline` and `trebi service logs --level error --lines 100 --grep <id>`. `service logs` is **local only** in follow/`-f` form; remote: `system.logs`. Do not `sqlite3` / `find` under `$TREBI_HOME`. See `trebi guide observability`.

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
