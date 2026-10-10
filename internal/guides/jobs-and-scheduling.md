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
  - event: discord
    rooms: ["1053685470757916764"]
    from: bots
    contains: failure
    replies: continue
skills: [my-skill]
artifacts:
  - path: report.html
    name: weekly-sales
    title: Weekly sales report
description: What this job does.
---

The prompt body. This is what the harness receives.
```

- `triggers[]` is the trebi form. Do not use a top-level `schedule:`.
- `match` and `extract` are JavaScript, evaluated over the event as `e`. Both are optional. An event trigger with neither takes every event that passes the other checks.
- An event trigger checks the event in this order: `rooms` (room IDs of the connector; empty takes every room), `allowlist` (sender IDs; empty takes every sender), `from`, `contains`, `extract`, then `match`. A sender miss records `blocked`. Every other miss records `no_match`.
- `from` is `people`, `bots`, or `anyone` (default). It reads `sender.bot` of the event. A connector that does not send it makes every sender a person.
- `contains` is one string or a list. The trigger takes the event when the message text contains any entry. Case does not matter. Prefer `contains` and `from` to a `match` expression.
- `contains`, `from`, and `replies` apply only to an `event` entry. On a `cron` or `every` entry each one is a parse error.
- Each `trebi-connector/1` event gives the prompt built-in vars: `{{text}}`, `{{sender}}`, `{{sender_id}}`, `{{room}}`, `{{room_name}}`, `{{thread}}`, `{{message_id}}`, `{{reply_to}}`, and `{{connection}}`. An `extract` var with the same name wins.
- `when` is an inline shell gate: the job runs only when `match` over `out` is true.
- `limit` sets how often an event trigger starts a run: `N/<duration>`, or `off`. The default is `15/10m`. Events above the limit wait in order. When 10 events wait, Trebi drops new events and opens one inbox item.
- `session.if_running` tells Trebi what to do when a run of the job is still active: `queue` (default) starts the new run after the active run, `skip` drops it, and `replace` stops the active run.
- `session.continue` (for example `7d`) keeps a conversation going after its run ends. An event with the same conversation key continues the session of the last run when that run ended inside the window. Set `conversation_key` in the trigger `extract` (for example a thread id). Without it, all events of one thread, or of one room with no thread, share a conversation.
- `replies: continue` (job triggers only; an agent rejects it, because an agent keeps its own channel conversation) sends each later message of a conversation to the run that the first message started. The conversation is the thread, the reply chain, or the whole DM (`trebi guide connectors-and-rules`). A later message is a follow-up: it skips `from`, `contains`, and `match`, but `rooms`, `allowlist`, and `extract` still apply. A bot message in an open conversation does not run. The conversation stays open with no time limit. Set `session.continue` to add a limit; after it, the next message must pass the full trigger again as a new start.
- A follow-up resumes the session of the newest run with the text "New message in this conversation from {{sender}}:" and the message, then one line that names the connection, room, and thread for the history command of the connector. When the session cannot resume (session file gone, failed run, other harness), a fresh session gets the job prompt with the vars of the first run, the last 20 stored messages, that line, and the message.
- Known effects of `replies: continue`: the harness deletes old session files, so a late reply gets a fresh session that sees only the messages, not the earlier tool output. A resumed session keeps the job prompt of its first run; a prompt edit reaches only a fresh session. In a DM each message after the first is a follow-up, so `contains` and `match` do not apply again in that DM.
- `artifacts[]` names files of the working folder. At the end of a complete run, Trebi saves each one as a new version of an artifact, unless the run saved that name itself. `path` is required and stays inside the working folder. `name` defaults to the file name without the extension, and `title` to the name. A missing file sets the outcome note of the run and opens an inbox item (`trebi guide artifacts`).
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

## Rename

```bash
trebi --workspace reports job rename agents/cmo/weekly digest
```

Rename changes the last part of the name only (`agents/cmo/weekly` becomes `agents/cmo/digest`). The folder moves with its files. The job keeps its ID, schedules, and earlier runs. It refuses an existing target, a live run, and a job at the workspace root.

## Delete

Ask the user first. `--purge` removes the job folder; plain delete leaves supporting files (`files_remaining` may be set).

```bash
trebi --workspace reports job delete my-job
trebi --workspace reports job delete my-job --purge
```

## Job memory

Each job keeps a structured `MEMORY.md` next to `JOB.md`. It holds three sections in a fixed order: Context, Lessons, Log. Keep records in `trebi state`, not in memory. End each run with a Log entry that starts with `Next:`. The daemon injects the file into each launch and validates it at run completion. A v1 `## State` section is an error (`state_deprecated`): move its records to `trebi state` and its handoff to a `Next:` Log entry. An invalid file gets a fix message injected into the session, twice at most; the run then ends with a `memory_invalid` alert.

```bash
trebi job memory show my-job --json
trebi job memory validate my-job
trebi job memory set-context --body context.md my-job
trebi job memory lesson add --text "CSV comes zipped" --at 2026-09-12 my-job
trebi job memory lesson delete my-job <entry-id>
trebi job memory log add --text "ran the digest" my-job
trebi job memory log delete my-job <entry-id>
trebi job memory set --file ./MEMORY.md my-job
trebi job memory clear --force my-job
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
trebi state --owner my-job
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
