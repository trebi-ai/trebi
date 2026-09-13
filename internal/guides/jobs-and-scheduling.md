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
  if_running: skip
  resumable: "true"
triggers:
  - cron: "0 9 * * *"
    timezone: America/New_York
  - every: 15m
    anchor: "00:00"
  - event: slack
    match: "e.channel === 'ops' && /deploy/.test(e.text)"
    extract: "({ user: e.user, text: e.text })"
    debounce: 30s
    rate_limit: 10/1h
    when:
      run: "git -C /repo status --porcelain"
      match: "out.trim() === ''"
      timeout: 10s
skills: [my-skill]
allowlist: [Bash, Read, Edit]
description: What this job does.
---

The prompt body. This is what the harness receives.
```

- `triggers[]` is the trebi form. Do not use a top-level `schedule:`.
- `match` and `extract` are JavaScript, evaluated over the event as `e`.
- `when` is an inline shell gate: the job runs only when `match` over `out` is true.
- Only `harness`, `triggers`, `envset`, and `session.idle_timeout` are canonical frontmatter; keep other fields minimal.

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
(default), `assistant`, `agent`, or `all`; the `agent` and `assistant` kinds
list every workspace. Folder-scoped agents and the `--agent` owner field:
`trebi guide agents`.

## Launch

```bash
trebi job run my-job --workspace reports --json
trebi job run my-job --workspace reports --test --json
trebi job run my-job --workspace reports --extra-prompt "extra prompt" --json
```

`--test` is only on `job run` (and `assistant wake --test`). Do not expect it on `rules fire` or `assistant say`. Use `--test` before the first real launch **only if this session created or edited that JOB.md**.

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

Each job keeps a `MEMORY.md` ledger that survives between runs.

```bash
trebi job memory show --json my-job
trebi job memory add --text "Shipped v2" --date 2026-08-28 my-job
trebi job memory remove my-job <entry-id>
trebi job memory set --file ./MEMORY.md my-job
trebi job memory clear --force my-job
```

`--file` on `memory set` is **local only**; remote: pass `content`. `memory clear` is destructive — ask first.

Supporting files next to the job: `trebi guide files`.
