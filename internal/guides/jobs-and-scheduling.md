---
topic: jobs-and-scheduling
title: Create and schedule jobs
description: From a registered workspace to a job that runs on cron, interval, or events.
---

# Create and schedule jobs

A job is a `JOB.md`: YAML frontmatter plus a prompt body. See `trebi guide getting-started` for the first launch.

## JOB.md shape

```markdown
---
title: Nightly report
harness: claude
working_dir: /path/to/repo
envset: prod
env:
  LOG_LEVEL: debug
session:
  max_duration: 30m
  idle_timeout: 5m
  if_running: queue
  resumable: "true"
triggers:
  - cron: "0 9 * * *"
    timezone: America/New_York
  - every: 15m
  - event: slack
    match: "e.channel === 'ops' && /deploy/.test(e.text)"
    extract: "({ user: e.user, text: e.text })"
    rooms: ["C024BE91L"]
    allowlist: ["U012AB3CD"]
    limit: 5/1h
    when:
      run: "git -C /repo status --porcelain"
      match: "out.trim() === ''"
      timeout: 10s
skills: [my-skill]
description: What this job does.
---

The prompt body. This is what the harness receives.
```

- `triggers[]` is the trebi form. Do not use a top-level `schedule:`.
- `match` and `extract` are JavaScript, evaluated over the event as `e`.
- An event trigger checks the event in this order: `rooms` (room IDs of the connector; empty takes every room), `allowlist` (sender IDs; empty takes every sender), `extract`, then `match`. A room miss records `no_match`. A sender miss records `blocked`.
- `when` is an inline shell gate: the job runs only when `match` over `out` is true.
- `limit` sets how often an event trigger starts a run: `N/<duration>`, or `off`. The default is `15/10m`. Events above the limit wait in order. When 10 events wait, Trebi drops new events and opens one inbox item.
- `session.if_running` tells Trebi what to do when a run of the job is still active: `queue` (default) starts the new run after the active run, `skip` drops it, and `replace` stops the active run.
- `session.continue` (for example `7d`) keeps a conversation going after its run ends. An event with the same conversation key continues the session of the last run when that run ended inside the window. Set `conversation_key` in the trigger `extract` (for example a thread id). Without it, all events of one room share a conversation.
- Only `harness` (name, or a `{name, model, effort}` map), `triggers`, `envset` (a name or a list of names, applied in order), `env` (inline map), and `session.idle_timeout` are canonical frontmatter; keep other fields minimal.

## Create, inspect, validate

```bash
trebi job create my-job --prompt "Run the sync." --cron "0 3 * * *" --workspace reports
trebi job create my-job --prompt "Read it." --on mail --match '.subject' --workspace reports
trebi job create my-job --prompt "Run the sync." --cron "0 3 * * *" --validate
trebi job create my-job --file ./JOB.md --workspace reports
trebi job list --workspace reports --search sync --harness claude --json
trebi job list --workspace reports --json
trebi job list --workspace reports --kind all --detailed --json
trebi job list --workspace reports --kind agent --json
trebi job list --workspace reports --schedule --json
trebi job show my-job --workspace reports --json
trebi job validate my-job --workspace reports --json
```

`--prompt` with a trigger flag lets the daemon compose the front matter. Prefer
it over hand-written YAML. `--validate` checks the definition and writes
nothing. `--file` is **local only** (reads a path); remote: pass `content`.
Always `validate` after you write a `JOB.md` yourself. `--kind` picks `job`
(default), `agent`, or `all`; the `agent` kind lists every workspace. The
assistant is not a job; `trebi assistant sessions` lists its conversations.
Folder-scoped agents and the `--agent` owner field: `trebi guide agents`.

## Launch

```bash
trebi job run my-job --workspace reports --json
trebi job run my-job --workspace reports --test --json
trebi job run my-job --workspace reports --extra-prompt "extra prompt" --json
```

`--test` is only on `job run`. Do not expect it on `rules fire` or `assistant say`. Use `--test` before the first real launch **only if this session created or edited that JOB.md**.

```bash
trebi job run my-job --workspace reports --attach
```

`--attach` is **local only**; remote: poll `runs show` / `runs transcript`. See `trebi guide runs-and-debugging`.

```bash
trebi job status my-job --workspace reports --json
trebi --workspace reports job tail my-job --lines 400
```

`job status` and `job tail` are **local only** convenience; remote: `job show` plus `runs list --job my-job` / `runs logs`.

## Pause and schedule

```bash
trebi --workspace reports job pause my-job
trebi --workspace reports job unpause my-job
trebi job schedule my-job disable
trebi job schedule my-job enable
trebi job schedule my-job "0 9 * * *"
trebi job schedule my-job "@every 15m"
```

`job schedule` is **local only**; remote: `job patch` the triggers / `job pause`. Event triggers: `trebi guide connectors-and-rules`.

## Delete

Ask the user first. `--purge` removes the job folder; plain delete leaves supporting files (`files_remaining` may be set).

```bash
trebi --workspace reports job delete my-job
trebi --workspace reports job delete my-job --purge
```

## Job memory

Each job keeps a structured `MEMORY.md` next to `JOB.md`. It holds four sections in a fixed order: Context, State, Lessons, Log. The daemon injects the file into each launch and validates it at run completion. An invalid file gets a fix message injected into the session, twice at most; the run then ends with a `memory_invalid` alert.

```bash
trebi job memory show my-job --json
trebi job memory validate my-job
trebi job memory set-context --body context.md my-job
trebi job memory set-state --body state.md my-job
trebi job memory lesson add --text "CSV comes zipped" --at 2026-09-12 my-job
trebi job memory lesson delete my-job <entry-id>
trebi job memory log add --text "ran the digest" my-job
trebi job memory log delete my-job <entry-id>
trebi job memory set --file ./MEMORY.md my-job
trebi job memory clear --force my-job
```

Lessons stay for the life of the job. Log entries are capped by `memory.max_logs` (default 100); the daemon prunes the oldest. Tune the gate with `memory:` frontmatter: `max_logs`, `require_update`, `inject_logs`, `max_bytes`. `--body` and `--file` are **local only**; remote: pass `body` / `content`. `memory clear` is destructive — ask first.

Offline repair works with the daemon down: `trebi memory validate <path> --fix` migrates a legacy ledger or seeds the skeleton, and `trebi memory skeleton` prints the empty file.

Supporting files next to the job: `trebi guide files`.
