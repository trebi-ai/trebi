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
    when:
      run: "git -C /repo status --porcelain"
      match: "out.trim() === ''"
      timeout: 10s
skills: [my-skill]
artifacts:
  - path: report.html
    name: weekly-sales
    title: Weekly sales report
description: What this job does.
folder: Reports
---

The prompt body. This is what the harness receives.
```

- `working_dir` is the folder where runs start. `~` expands, and a relative path starts at the `JOB.md` folder. With no `working_dir`, a job that an agent owns uses the `working_dir` of its agent, else the `JOB.md` folder (`trebi guide agents`).
- A job has no event trigger. A route of a connection sends its events to a job or an agent. The filters `rooms`, `allowlist`, `from`, `contains`, `match`, `extract`, `replies`, `batch`, `chat`, and `limit` are route keys: see `trebi guide routing`.
- `triggers[]` holds schedules only: `cron` and `every`. An `event` entry is a parse error that names the routes file. Do not use a top-level `schedule:`.
- `when` is an inline shell gate on a schedule entry: the job runs only when `match` over `out` is true.
- `session.if_running` tells Trebi what to do when a run of the job is still active: `queue` (default) starts the new run after the active run, `skip` drops it, and `replace` stops the active run.
- `session.continue` (for example `7d`) keeps a conversation going after its run ends. An event with the same conversation key continues the session of the last run when that run ended inside the window. Set `conversation_key` in the route `extract` (for example a thread id). Without it, all events of one thread, or of one room with no thread, share a conversation. An agent also takes `session.continue`: a message in its channel conversation then continues the last run as a new turn, not a new run with a recap.
- `artifacts[]` names files of the working folder. At the end of a complete run, Trebi saves each one as a new version of an artifact, unless the run saved that name itself. `path` is required and stays inside the working folder. `name` defaults to the file name without the extension, and `title` to the name. A missing file sets the outcome note of the run and opens an inbox item (`trebi guide artifacts`).
- `folder` groups a job under its owner agent in the UI. Use one name of at most 64 characters, with no `/`. The match is case-sensitive, and an empty value is no folder.
- A job with no `harness` uses the harness of its agent, then the `defaults.command` harness. `model` and `effort` come from the agent too, but only when the job and the agent use the same harness. Set `harness` on a job only when the job needs another harness than its agent. `trebi job patch <name> harness=` removes the job value.
- Only `harness` (name, or a `{name, model, effort}` map), `triggers`, `env` (inline map), and `session.idle_timeout` are canonical frontmatter; keep other fields minimal.
- To run a job with other accounts, services, or models of the same program, make a harness variant (`trebi harness add`) and set `harness` to its name. A variant keeps its own env and secrets. `env` adds values for one job, and a job value wins over a harness value.

## Create, inspect, validate

Each job belongs to one agent and lives in `agents/<agent>/<job>/`. `--agent` is required. A `JOB.md` outside an agent folder is a parse error: it does not run, and `trebi doctor` names it. The name of a job is `<agent>/<job>`. The name of a job outside the agent layout is `/` plus its folder, for example `/reports/digest`. When the workspace has no agent, make one first (`trebi guide agents`).

```bash
trebi job create my-job --agent ops --prompt "Run the sync." --cron "0 3 * * *" --workspace reports
trebi job create my-job --agent ops --prompt "Run the sync." --cron "0 3 * * *" --validate
trebi job create my-job --agent ops --file ./JOB.md --workspace reports
trebi job list --workspace reports --search sync --harness claude --json
trebi job list --workspace reports --json
trebi job list --workspace reports --kind all --detailed --json
trebi job list --workspace reports --kind agent --json
trebi job list --workspace reports --schedule --json
trebi job show ops/my-job --workspace reports --json
trebi job validate ops/my-job --workspace reports --json
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
trebi job run ops/my-job --workspace reports --json
trebi job run ops/my-job --workspace reports --test --json
trebi job run ops/my-job --workspace reports --extra-prompt "extra prompt" --json
```

`--test` is only on `job run`. Do not expect it on `rules fire` or `assistant say`. Use `--test` before the first real launch **only if this session created or edited that JOB.md**.

```bash
trebi job run ops/my-job --workspace reports --attach
```

`--attach` is **local only**; remote: poll `runs show` / `runs transcript`. See `trebi guide runs-and-debugging`.

## Inputs and hand runs

A job declares its inputs in `inputs:`, in form order. Each `{{name}}` in the prompt gets the value:

```markdown
---
inputs:
  body: {type: string, label: Email text, format: multiline, required: true}
  tone: {type: string, enum: [friendly, formal, short], default: friendly}
---

Draft a {{tone}} reply to: {{body}}
```

A route can map event fields to inputs, and `gets: by_hand` makes it wait for a person. The route is in `routes/gmail.yaml` in the workspace folder:

```yaml
routes:
  - job: reply-agent/reply
    gets: by_hand
    types: [mail.received]
    inputs: {body: "{{data.body}}"}
```

Types are `string`, `number`, `integer`, and `boolean`. An input that the route does not map takes `data.<name>` of the event, then its default. `job validate` lists `issues` with a `path` and a `code`.

```bash
trebi job run reply --workspace reports --input tone=formal --input body="Can we meet?" --json
trebi event list --outcome manual --json                 # events that wait for a person
trebi event jobs <event-id> --json                        # jobs that can run with the event
trebi job run reply --workspace reports --event <event-id> --test --json   # see each input {value, source}
trebi event run <event-id> --job reply --workspace reports --input tone=short
trebi job simulate reply --workspace reports --since 24h --preview --json  # test against stored events; starts no run
```

A run with `--event` needs a route of the job that takes the event (422 `event_not_accepted`). `--route <id>` picks the route; `--force` skips the route checks. A required input with no value gives 422 `invalid` with `details.fields`.

```bash
trebi job status ops/my-job --workspace reports --json
trebi --workspace reports job tail ops/my-job --lines 400
```

`job status` and `job tail` are **local only** convenience; remote: `job show` plus `runs list --job ops/my-job` / `runs logs`.

## Pause and schedule

```bash
trebi --workspace reports job pause ops/my-job
trebi --workspace reports job unpause ops/my-job
trebi job schedule ops/my-job disable
trebi job schedule ops/my-job enable
trebi job schedule ops/my-job "0 9 * * *"
trebi job schedule ops/my-job "@every 15m"
```

`job schedule` is **local only**; remote: `job patch` the triggers / `job pause`. Event routes: `trebi guide routing`.

## Archive

An archived job keeps its files, runs, state, and memory, but it does not start. Archive a job that you no longer need and do not want to delete.

```bash
trebi --workspace reports job archive ops/my-job
trebi --workspace reports job unarchive ops/my-job
trebi --workspace reports job list --archived
trebi --workspace reports job list --include-archived
```

Archive writes `archived_at` in `JOB.md`. The job leaves the default list, the schedule, the routes, and the doctor checks. A run by hand, a replay, and a start by an event fail with the code `archived`. A dry run still works and warns. Open inbox items of the job close, and its waiting queue items are skipped. A live run continues. Unarchive keeps the on or off switch of the job, and the schedule starts again with no catch-up. A job in an archived agent cannot be unarchived alone: unarchive the agent (`trebi agent unarchive`) or `trebi job move` the job to another agent. An agent run can archive its own jobs.

## Pause in bulk

A pause holds the runs of many jobs at once and keeps the on or off switch of each job. Pause everything, one workspace, one harness, or one agent with its jobs:

```bash
trebi pause harness claude --until reset         # until the usage limit resets
trebi pause workspace reports --for 2d --missed once
trebi --workspace reports pause agent ops --until "2026-11-10 09:00"
trebi pause all --no-end --note "travel"
trebi pause ls [--all] [--json]
trebi pause set <pause-id> --for 3h
trebi pause resume <pause-id> | all | workspace reports | harness claude
```

- A scheduled run in a pause is skipped. `--missed once` runs each job that missed a run one time when the pause ends.
- An event run waits in the queue and runs when the pause ends.
- A run that you start by hand runs, with a warning. A live run continues. The assistant is never paused.
- `trebi job list --json` and `trebi agent list --json` give `held_by` and the status `held`.
- Trebi pauses a harness itself when a usage window reaches its `pause_at` percent (default 99), until the window resets. `trebi harness set claude --pause-at 95` changes it; `--pause-at off` turns it off. Resume such a pause early and Trebi does not pause that harness again until that reset.
- An agent run cannot pause or resume. The MCP `pause` tool has the same actions.

## Rename

```bash
trebi --workspace reports job rename cmo/weekly digest
```

Rename changes the last part of the name only (`cmo/weekly` becomes `cmo/digest`). The folder moves with its files. The job keeps its ID, schedules, and earlier runs. It refuses an existing target, a live run, and a job at the workspace root.

## Move

```bash
trebi --workspace reports job move cmo/renewals --agent cs --folder Health --dry-run
trebi --workspace reports job move cmo/renewals --agent cs --folder Health
trebi --workspace reports job move cmo/digest --folder Content
trebi --workspace reports job move /reports/digest --agent cmo
trebi --workspace reports agent folder rename cmo Customers Health
```

Move gives a job another agent, another folder, or both. A job always belongs to an agent, so a move always names one or keeps the owner. Move a job outside an agent folder into an agent to fix its parse error. Always use `job move`; never move a job folder on disk by hand. With a new agent, the folder moves to `agents/<agent>/<last part>`. The job keeps its ID, schedules, state, memory, and earlier runs. The job then runs with the settings, connectors, and email address of the new agent. Run `--dry-run` first and show the `changes` list to the user. `agent folder rename` moves every job of an agent from one folder to another; an empty target removes the folder.

## Delete

Ask the user first. `--purge` removes the job folder; plain delete leaves supporting files (`files_remaining` may be set).

```bash
trebi --workspace reports job delete ops/my-job
trebi --workspace reports job delete ops/my-job --purge
```

## Job memory

Each job keeps a structured `MEMORY.md` next to `JOB.md`. It holds three sections in a fixed order: Context, Lessons, Log. Keep records in `trebi state`, not in memory. End each run with a Log entry that starts with `Next:`. The daemon injects the file into each launch and validates it at run completion. A v1 `## State` section is an error (`state_deprecated`): move its records to `trebi state` and its handoff to a `Next:` Log entry. Trebi removes the oldest Log entries over the caps itself. A file that is still invalid gets a fix message injected into the session, five times at most. A file that stays damaged for three runs in a row raises a `memory_invalid` alert; a file that only missed its Log entry does not. A later run with a sound file closes the alert.

```bash
trebi job memory show ops/my-job --json
trebi job memory validate ops/my-job
trebi job memory set-context --body context.md ops/my-job
trebi job memory lesson add --text "CSV comes zipped" --at 2026-09-12 ops/my-job
trebi job memory lesson delete ops/my-job <entry-id>
trebi job memory log add --text "ran the digest" ops/my-job
trebi job memory log delete ops/my-job <entry-id>
trebi job memory set --file ./MEMORY.md ops/my-job
trebi job memory clear --force ops/my-job
```

Lessons stay for the life of the job. Log entries are capped by `memory.max_logs` (default 100); the daemon prunes the oldest. `max_bytes` caps the memory part of the prompt (Context, all Lessons, and the newest `inject_logs` Log entries); the default is 16384 (`defaults.memory.max_bytes`), and 0 turns it off. Over the cap, the gate tells the session what to trim. Tune the gate with `memory:` frontmatter: `max_logs`, `require_update`, `inject_logs`, `max_bytes`. `--body` and `--file` are **local only**; remote: pass `body` / `content`. `memory clear` is destructive — ask first.

Offline repair works with the daemon down: `trebi memory validate <path> --fix` migrates a legacy ledger or seeds the skeleton, and `trebi memory skeleton` prints the empty file.

## Job state

State keeps the records of a job: items already sent, sources, scores. A record is a row with a key, an optional time `at`, and a JSON object value. Rows live in named collections. Each launch gets a short `## State` brief with each collection, its row count, its fields, and the changes of the last run. Declare collections in the frontmatter to add a description, a TTL, and a JSON Schema check:

```yaml
state:
  reported:
    description: Items already sent.
    ttl: 2y
    schema:
      type: object
      required: [title]
```

An undeclared collection also works. In a run, the owner is `self` by default. Outside a run, pass `--owner <job id or name>`.

```bash
trebi state --owner ops/my-job
trebi state query reported --since 7d --where "v.score == 2" --limit 20
trebi state has reported doi:10/1 doi:10/2
trebi state get reported doi:10/1
trebi state put reported doi:10/1 --value '{"title":"A paper"}' --at today
trebi state put reported --mode merge --file rows.jsonl
trebi state prune reported --until 180d --yes
trebi state delete reported doi:10/1 --yes
trebi state drop reported --yes
```

A batch writes all rows or none. A bad batch prints one line for each failed row, for example `line 3: value.topic: ...`. Times take `2026-09-01`, `7d`, `today`, or `last-run`. `delete`, `prune`, and `drop` are destructive: outside a run, ask the user first, then pass `--yes`. A run deletes its own rows with no confirm. Limits: `defaults.state.max_rows` and `defaults.state.max_bytes` for each owner. The state of a deleted job stays 30 days, then the daemon deletes it.

Supporting files next to the job: `trebi guide files`.
