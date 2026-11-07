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
  - event: discord
    rooms: ["1053685470757916764"]
    from: bots
    contains: failure
    replies: continue
  - event: whatsapp
    batch: 15m
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
- `triggers[]` is the trebi form. Do not use a top-level `schedule:`.
- `match` and `extract` are JavaScript, evaluated over the event as `e`. Both are optional. An event trigger with neither takes every event that passes the other checks.
- An event trigger checks the event in this order: `rooms` (room IDs of the connector; empty takes every room), `allowlist` (sender IDs; empty takes every sender), `from`, `contains`, `extract`, then `match`. A sender miss records `blocked`. Every other miss records `no_match`.
- `from` is `people`, `bots`, or `anyone` (default). It reads `sender.bot` of the event. A connector that does not send it makes every sender a person.
- `contains` is one string or a list. The trigger takes the event when the message text contains any entry. Case does not matter. Prefer `contains` and `from` to a `match` expression.
- `contains`, `from`, `replies`, and `batch` apply only to an `event` entry. On a `cron` or `every` entry each one is a parse error.
- Each `trebi-connector/1` event gives the prompt built-in vars: `{{text}}`, `{{sender}}`, `{{sender_id}}`, `{{room}}`, `{{room_name}}`, `{{thread}}`, `{{message_id}}`, `{{reply_to}}`, and `{{connection}}`. An `extract` var with the same name wins.
- `when` is an inline shell gate: the job runs only when `match` over `out` is true.
- `limit` sets how often an event trigger starts a run: `N/<duration>`, or `off`. The default is `15/10m`. Events above the limit wait in order. When 10 events wait, Trebi drops new events and opens one inbox item.
- `session.if_running` tells Trebi what to do when a run of the job is still active: `queue` (default) starts the new run after the active run, `skip` drops it, and `replace` stops the active run.
- `session.continue` (for example `7d`) keeps a conversation going after its run ends. An event with the same conversation key continues the session of the last run when that run ended inside the window. Set `conversation_key` in the trigger `extract` (for example a thread id). Without it, all events of one thread, or of one room with no thread, share a conversation. An agent also takes `session.continue`: a message in its channel conversation then continues the last run as a new turn, not a new run with a recap.
- `replies: continue` (job triggers only; an agent rejects it, because an agent keeps its own channel conversation) sends each later message of a conversation to the run that the first message started. The conversation is the thread, the reply chain, or the whole DM (`trebi guide connectors-and-rules`). A later message is a follow-up: it skips `from`, `contains`, and `match`, but `rooms`, `allowlist`, and `extract` still apply. A bot message in an open conversation does not run. The conversation stays open with no time limit. Set `session.continue` to add a limit; after it, the next message must pass the full trigger again as a new start.
- A follow-up resumes the session of the newest run with the text "New message in this conversation from {{sender}}:" and the message, then one line that names the connection, room, and thread for the history command of the connector. When the session cannot resume (session file gone, failed run, other harness), a fresh session gets the job prompt with the vars of the first run, the last 20 stored messages, that line, and the message.
- Known effects of `replies: continue`: the harness deletes old session files, so a late reply gets a fresh session that sees only the messages, not the earlier tool output. A resumed session keeps the job prompt of its first run; a prompt edit reaches only a fresh session. In a DM each message after the first is a follow-up, so `contains` and `match` do not apply again in that DM.
- `batch` (for example `15m`; from `1m` to `24h`; `off` or empty is no batch) collects the events of the trigger and starts one run per interval with all of them. See "Batch events" below.
- `artifacts[]` names files of the working folder. At the end of a complete run, Trebi saves each one as a new version of an artifact, unless the run saved that name itself. `path` is required and stays inside the working folder. `name` defaults to the file name without the extension, and `title` to the name. A missing file sets the outcome note of the run and opens an inbox item (`trebi guide artifacts`).
- `folder` groups a job under its owner agent in the UI. Use one name of at most 64 characters, with no `/`. The match is case-sensitive, and an empty value is no folder.
- A job with no `harness` uses the harness of its agent, then the `defaults.command` harness. `model` and `effort` come from the agent too, but only when the job and the agent use the same harness. Set `harness` on a job only when the job needs another harness than its agent. `trebi job patch <name> harness=` removes the job value.
- Only `harness` (name, or a `{name, model, effort}` map), `triggers`, `env` (inline map), and `session.idle_timeout` are canonical frontmatter; keep other fields minimal.
- To run a job with other accounts, services, or models of the same program, make a harness variant (`trebi harness add`) and set `harness` to its name. A variant keeps its own env and secrets. `env` adds values for one job, and a job value wins over a harness value.

## Batch events

`batch: <duration>` on an event trigger collects the events that match and starts one run per interval with all of them. Use it when the user asks for "at most once every 15 minutes with all new messages". `--batch 15m` on `trebi job create` writes it.

- The interval is from `1m` to `24h`. Ticks fall at the daemon start time plus a whole number of intervals. A tick with no events starts no run.
- The prompt gets `{{events}}` (one line per event, at most 50, then "… and N more in <file>"), `{{event_count}}`, and `{{events_file}}` (a JSON file with every event of the batch, at `~/.trebi/runs/<run-id>/events.json`). `{{connection}}` is set when all events come from one connection.
- A prompt with no `{{events}}` gets this block at its end: `New events (<n>):`, the event lines, and `All events: <file>`. An agent gets the same block as its wake message.
- Single-event vars such as `{{text}}` or `{{sender}}` are empty in a batch run. `trebi job validate` warns about them.
- One batch holds at most 500 events. The event that fills it starts the run at once. `limit` and `session.if_running` apply to each batch run, and new events still join a batch that waits.
- `batch` cannot be used with `replies: continue`. The assistant rejects `batch` on its own triggers.
- Each event keeps its own outcome. `trebi queue ls` shows the event count of each batch, and `trebi runs show` shows `trigger.batch` with the event ids.

```bash
trebi job create digest --agent cmo --prompt "Summarize the new messages." --on whatsapp --batch 15m --workspace reports
trebi rules test --event-id <event-id> --batch 15m --json
```

## Create, inspect, validate

Each job belongs to one agent and lives in `agents/<agent>/<job>/`. `--agent` is required. A `JOB.md` outside an agent folder is a parse error: it does not run, and `trebi doctor` names it. When the workspace has no agent, make one first (`trebi guide agents`).

```bash
trebi job create my-job --agent ops --prompt "Run the sync." --cron "0 3 * * *" --workspace reports
trebi job create my-job --agent ops --prompt "Read it." --on mail --match '.subject' --workspace reports
trebi job create my-job --agent ops --prompt "Run the sync." --cron "0 3 * * *" --validate
trebi job create my-job --agent ops --file ./JOB.md --workspace reports
trebi job list --workspace reports --search sync --harness claude --json
trebi job list --workspace reports --json
trebi job list --workspace reports --kind all --detailed --json
trebi job list --workspace reports --kind agent --json
trebi job list --workspace reports --schedule --json
trebi job show agents/ops/my-job --workspace reports --json
trebi job validate agents/ops/my-job --workspace reports --json
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
trebi job run agents/ops/my-job --workspace reports --json
trebi job run agents/ops/my-job --workspace reports --test --json
trebi job run agents/ops/my-job --workspace reports --extra-prompt "extra prompt" --json
```

`--test` is only on `job run`. Do not expect it on `rules fire` or `assistant say`. Use `--test` before the first real launch **only if this session created or edited that JOB.md**.

```bash
trebi job run agents/ops/my-job --workspace reports --attach
```

`--attach` is **local only**; remote: poll `runs show` / `runs transcript`. See `trebi guide runs-and-debugging`.

## Inputs and hand runs

A job declares its inputs in `inputs:`, in form order. Each `{{name}}` in the prompt gets the value. An event trigger can map event fields to inputs, and `manual: true` makes it wait for a person:

```markdown
---
inputs:
  body: {type: string, label: Email text, format: multiline, required: true}
  tone: {type: string, enum: [friendly, formal, short], default: friendly}
triggers:
  - event: gmail
    types: [mail.received]
    manual: true
    inputs: {body: "{{data.body}}"}
---

Draft a {{tone}} reply to: {{body}}
```

Types are `string`, `number`, `integer`, and `boolean`. An input that the trigger does not map takes `data.<name>` of the event, then its default. `job validate` lists `issues` with a `path` and a `code`.

```bash
trebi job run reply --workspace reports --input tone=formal --input body="Can we meet?" --json
trebi event list --outcome manual --json                 # events that wait for a person
trebi event jobs <event-id> --json                        # jobs that can run with the event
trebi job run reply --workspace reports --event <event-id> --test --json   # see each input {value, source}
trebi event run <event-id> --job reply --workspace reports --input tone=short
trebi job simulate reply --workspace reports --since 24h --preview --json  # test against stored events; starts no run
```

A run with `--event` needs an event trigger of the job that takes the event (422 `event_not_accepted`); `--force` skips the trigger checks. A required input with no value gives 422 `invalid` with `details.fields`.

```bash
trebi job status agents/ops/my-job --workspace reports --json
trebi --workspace reports job tail agents/ops/my-job --lines 400
```

`job status` and `job tail` are **local only** convenience; remote: `job show` plus `runs list --job agents/ops/my-job` / `runs logs`.

## Pause and schedule

```bash
trebi --workspace reports job pause agents/ops/my-job
trebi --workspace reports job unpause agents/ops/my-job
trebi job schedule agents/ops/my-job disable
trebi job schedule agents/ops/my-job enable
trebi job schedule agents/ops/my-job "0 9 * * *"
trebi job schedule agents/ops/my-job "@every 15m"
```

`job schedule` is **local only**; remote: `job patch` the triggers / `job pause`. Event triggers: `trebi guide connectors-and-rules`.

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
trebi --workspace reports job rename agents/cmo/weekly digest
```

Rename changes the last part of the name only (`agents/cmo/weekly` becomes `agents/cmo/digest`). The folder moves with its files. The job keeps its ID, schedules, and earlier runs. It refuses an existing target, a live run, and a job at the workspace root.

## Move

```bash
trebi --workspace reports job move agents/cmo/renewals --agent cs --folder Health --dry-run
trebi --workspace reports job move agents/cmo/renewals --agent cs --folder Health
trebi --workspace reports job move agents/cmo/digest --folder Content
trebi --workspace reports job move reports/digest --agent cmo
trebi --workspace reports agent folder rename cmo Customers Health
```

Move gives a job another agent, another folder, or both. A job always belongs to an agent, so a move always names one or keeps the owner. Move a job outside an agent folder into an agent to fix its parse error. Always use `job move`; never move a job folder on disk by hand. With a new agent, the folder moves to `agents/<agent>/<last part>`. The job keeps its ID, schedules, state, memory, and earlier runs. The job then runs with the settings, connectors, and email address of the new agent. Run `--dry-run` first and show the `changes` list to the user. `agent folder rename` moves every job of an agent from one folder to another; an empty target removes the folder.

## Delete

Ask the user first. `--purge` removes the job folder; plain delete leaves supporting files (`files_remaining` may be set).

```bash
trebi --workspace reports job delete agents/ops/my-job
trebi --workspace reports job delete agents/ops/my-job --purge
```

## Job memory

Each job keeps a structured `MEMORY.md` next to `JOB.md`. It holds three sections in a fixed order: Context, Lessons, Log. Keep records in `trebi state`, not in memory. End each run with a Log entry that starts with `Next:`. The daemon injects the file into each launch and validates it at run completion. A v1 `## State` section is an error (`state_deprecated`): move its records to `trebi state` and its handoff to a `Next:` Log entry. Trebi removes the oldest Log entries over the caps itself. A file that is still invalid gets a fix message injected into the session, five times at most. A file that stays damaged for three runs in a row raises a `memory_invalid` alert; a file that only missed its Log entry does not. A later run with a sound file closes the alert.

```bash
trebi job memory show agents/ops/my-job --json
trebi job memory validate agents/ops/my-job
trebi job memory set-context --body context.md agents/ops/my-job
trebi job memory lesson add --text "CSV comes zipped" --at 2026-09-12 agents/ops/my-job
trebi job memory lesson delete agents/ops/my-job <entry-id>
trebi job memory log add --text "ran the digest" agents/ops/my-job
trebi job memory log delete agents/ops/my-job <entry-id>
trebi job memory set --file ./MEMORY.md agents/ops/my-job
trebi job memory clear --force agents/ops/my-job
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
trebi state --owner agents/ops/my-job
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
