---
name: trebi
description: Operate trebi, an LLM task runner (daemon + CLI), fully from the shell. Use when the user wants to create, run, schedule, or debug trebi jobs; put supporting files next to a job; inspect or attach to runs; manage workspaces, skills, secrets, integrations, and event rules; drain the inbox or alerts; tail the event bus; or control the trebid daemon. Triggers: "trebi job", "run a trebi job", "schedule a job", "trebi job file", "put a script next to a job", "trebi file", "trebi runs", "attach to a run", "trebi daemon", "trebi workspace", "trebi inbox", "trebi bus", "trebi integration", "trebi doctor", "wake the assistant", "trebi assistant say", "trebi assistant chat", "trebi conversation".
---

# Trebi

Trebi is one binary that is both a daemon (`trebid`) and a CLI. The daemon owns an HTTP `/v1` API, an SSE event bus, and a SQLite store under `~/.trebi`. The CLI is a thin client of that API and covers 100% of it. Drive trebi only through `trebi`. Do not edit files under `~/.trebi` and do not call the API with `curl`.

## Object model

| Object | What it is |
|---|---|
| workspace | a registered folder; the unit of context. Jobs, runs, and the assistant belong to one |
| job | a `JOB.md` file: YAML frontmatter (harness, triggers, session) plus a prompt body |
| file | any path inside a workspace (scripts, templates, eval files). Raw bytes; no job semantics. Job-scoped via `trebi job file`; workspace-wide via `trebi file` |
| run | one launch of a job in a harness session (tmux locally) |
| assistant | the singleton `TREBI.md` job of a workspace, kind=assistant |
| conversation | a chat thread with the assistant; the CLI/UI are channels onto it |
| integration | an external event producer (a `stream`, `cli`, or `mcp` command) |
| trigger | a rule on a job: `cron`, `every`, or `event` from an integration |
| skill | a stored `SKILL.md` the harness can load |
| inbox | the queue you drain: approvals, failures, health, watchdog |
| alerts | the per-firing alert feed; a log, not a queue |
| queue | the dispatch queue of pending launches |
| bus | the live event stream (`trebi bus`) |

## First checks

Run these before anything else. Most failures are a daemon that is down.

```bash
trebi service status --json      # OS service + API health
trebi doctor --json              # findings; add --fix to repair automatically
trebi workspace current --json   # the sticky workspace context
```

If `doctor` is healthy, stop. Do not "fix" the database, edit `$TREBI_HOME`, or send signals to trebid.

If the daemon is down: `trebi service start`. If it was never installed: `trebi service install` then `trebi service start`. Never run `trebi service daemon` yourself and never `kill` the daemon PID. `service daemon` is a foreground dev entrypoint; `start`/`stop` own the process.

Only these work with the daemon down: `service install|start|stop|status`, `doctor`, `config validate`. Everything else fails with a clear message.

## Global rules

- `--workspace` / `-w` (or `$TREBI_WORKSPACE`) selects the workspace. Put it **before** the verb: `trebi -w staging job run report`. urfave does not parse flags after a positional name (`trebi job run report -w staging` fails). `job run` / `job show` / `job status` / `job validate` / `runs list` also accept `--workspace` **before** the name: `trebi job run --workspace staging report`.
- Without `-w`, trebi uses the sticky context from `trebi workspace use`.
- Every read command takes `--json`. Use `--json` and pipe to `jq` when you parse output. Never parse the human tables.
- List commands paginate with `--limit` and `--cursor`. The cursor is the id of the last row. `trebi service logs` is not a list command: it uses `--lines` / `-n` and `--grep`, not `--limit`.
- Times are RFC3339 UTC. Durations end in `_ms`. Keys are snake_case.
- The server computes `duration_ms`, `cost_usd`, and `billed`. Never recompute money or duration yourself.

## Workspaces

```bash
trebi workspace list                       # local + linked remote
trebi workspace add --name reports --create --use   # remote default; lands in workspaces.root (~/trebi)
trebi workspace add /path/to/repo --use    # register an existing folder
trebi workspace add --name proj --url https://host:7433 --token-stdin --use
trebi workspace use <id|name>              # set sticky context
trebi workspace current
trebi workspace rename <id> <new-name>
trebi workspace set <id> --name new --git-enabled
trebi workspace unlink <id|name>           # drop a remote link
trebi workspace remove <id> --force        # delete folder + unregister
```

Pathless `--create --name` is the remote default: the daemon mkdirs `<workspaces.root>/<name>` (config key `workspaces.root`, default `~/trebi`). Pass an absolute `--create` path only when you know the server filesystem. A remote workspace is context, not a flag: link it, then `use` it, and every command routes to that daemon. Client resolution order is fixed: linked remote → remote sticky context → `TREBI_URL`/`TREBI_TOKEN` → local `~/.trebi/api.json`.

## Jobs

A job is a `JOB.md`. Create one from a file or stdin:

```bash
trebi job create my-job --file ./JOB.md
cat JOB.md | trebi job create my-job --workspace <id>
```

### JOB.md format

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
  if_running: skip        # skip | queue | kill
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

- `triggers[]` is the trebi form. It is not jobi's `schedule:`.
- `match` and `extract` are JavaScript, evaluated over the event as `e`.
- `when` is an inline shell gate: the job runs only when `match` over `out` is true.
- Only `harness`, `triggers`, `envset`, and `session.idle_timeout` are canonical frontmatter; keep other fields minimal.

### Job lifecycle

```bash
trebi job list                          # add --schedule for SPEC/NEXT/LAST columns
trebi job list --kind all --detailed
trebi job show <name>
trebi job status <name>                 # job + latest run summary
trebi job validate <name>               # check frontmatter before running
trebi -w staging job run <name>         # launch in a named workspace
trebi job run --workspace staging <name>
trebi job run <name>                    # launch now
trebi job run <name> --test             # dry run: evaluate gates, print the plan
trebi job run <name> -e "extra prompt"  # append to the prompt
trebi job run <name> --attach           # launch, then attach to the session
trebi job tail <name> -n 400            # tail the latest run log
trebi job pause <name> / unpause <name>
trebi job schedule <name> disable       # or: enable | "0 9 * * *" | @every 15m
trebi job delete <name>                 # removes JOB.md; supporting files remain
trebi job delete <name> --purge         # removes the job folder
```

`--purge` is how you end a project remotely. Plain delete leaves scripts/templates so the job can be recreated over them; the response may include `files_remaining`. Always `trebi job validate` after you write a `JOB.md`, and prefer `trebi job run <name> --test` before the first real launch. `--test` is only on `job run` (and `assistant wake --test`). Do not expect it on `rules fire` or `assistant say`.

### Job memory

Each job keeps a `MEMORY.md` ledger that survives between runs.

```bash
trebi job memory show <name>
trebi job memory add <name> --text "Shipped v2" --date 2026-08-28
trebi job memory remove <name> <entry-id>
trebi job memory set <name> --file ./MEMORY.md   # replace whole file
trebi job memory clear <name>                    # destructive
```

## Files

Supporting files for a job (Python scripts, HTML templates, eval fixtures) go next to it. Use `trebi job file` — the default. Paths are job-relative (`render.py`, not `report/render.py`). A wrong prefix cannot touch a sibling job.

`JOB.md` / `MEMORY.md` / `TREBI.md` writes on this command 409: edit those with `trebi job` and `trebi job memory`.

```bash
trebi job file ls   <job> [prefix] --json --limit 100
trebi job file get  <job> render.py                  # bytes to stdout
trebi job file get  <job> render.py --out ./render.py
trebi job file get  <job> render.py --stat           # JSON: path, size_bytes, mtime, hash
trebi job file put  <job> render.py --from ./render.py
cat render.py | trebi job file put <job> render.py
trebi job file put  <job> render.py --from ./render.py --if-match <hash>
trebi job file rm   <job> render.py --if-match <hash>
```

`--workspace` disambiguates a duplicate job name, same as `trebi job show`. `put` is a full replace. Re-read (`--stat`) after a 412 `precondition_failed`.

`trebi file` is workspace-level: use only for assets shared across jobs (`templates/`, `lib/`) or assistant support files. Same If-Match / size-cap rules. `JOB.md` / `TREBI.md` / `MEMORY.md` are allowed there — the index rebuilds the same way as an editor save. `trebi.yaml` and `.git/**` are denied.

```bash
trebi file ls [prefix] --json --limit 100
trebi file get templates/base.html
trebi file put templates/base.html --from ./base.html
trebi file rm  templates/base.html --if-match <hash>
```

## Runs

```bash
trebi runs list                                    # add --json
trebi -w staging runs list --job report --json
trebi runs list --workspace staging --job report --json
trebi runs list --status running --job my-job --limit 50
trebi runs list --harness claude --model <model> --kind assistant
trebi runs show <id>
trebi runs timeline <id>                           # ordered events of the run
trebi runs transcript <id>                         # parsed turns (JSON)
trebi runs transcript <id> --raw                   # raw ndjson from the harness
trebi runs stats                                   # totals and currently running
trebi runs agents                                  # live in-memory agent state
trebi runs attach <id>                             # tmux locally, WebSocket remotely
trebi runs message <id> "answer to the prompt"     # send text to a live run
trebi runs kill <id> [<id>...]
trebi runs resume <id>                             # resume the prior session
trebi runs replay <id>                             # new launch from the prior prompt
trebi runs retry <id>                              # resume, park, or rerun by end_reason
```

`resume` continues the same session. `replay` starts a fresh run from the same prompt. `retry` picks the right one from the failure reason — prefer it after a failure.

Approvals block a run until you decide:

```bash
trebi runs approvals --json
trebi runs approve <id> --decision allow    # or: deny
```

## Assistant

The assistant is the workspace singleton driven by `TREBI.md`.

```bash
trebi assistant show
trebi assistant wake                        # launch a kind=assistant run
trebi assistant wake --test                 # evaluate gates only
trebi assistant wake -e "focus on the CI failures" --attach
trebi assistant say "what is blocked?"      # chat send; prints delivery=injected|queued
trebi assistant chat                        # REPL: POST + SSE refetch; /new /switch /quit
trebi assistant history --limit 20 --json
trebi assistant memory show
trebi assistant memory add --text "Reviewed the backlog"
trebi assistant memory set --file ./MEMORY.md
trebi assistant memory remove <entry-id>
trebi assistant memory clear
```

Conversations are the chat object. The CLI channel pointer is per workspace.

```bash
trebi conversation list --json
trebi conversation show <id>
trebi conversation messages <id> --json
trebi conversation new --title "CI firefight"
trebi conversation switch <id>
```

## Inbox and alerts

The inbox is a queue you drain. Alerts are a read-only feed of every firing.

```bash
trebi inbox list --status unread --json
trebi inbox list --kind approval        # approval | failure | health | watchdog
trebi inbox show <id>
trebi inbox read <id>
trebi inbox read-all
trebi inbox resolve <id>                # manually dismiss
trebi alerts --since 2026-08-01T00:00:00Z --json
```

## Queue

```bash
trebi queue --status pending --json
trebi queue cancel <id>
trebi queue requeue <id>       # move a blocked/done row back to pending
```

## Integrations and event rules

An integration is a command that produces events.

```bash
trebi integration list --json
trebi integration set slack --type stream --format json --cwd /opt/slack \
  -e TOKEN=xxx -- ./slack-stream --follow
trebi integration show <name>
trebi integration test <name>
trebi integration connect <name> / disconnect <name>
trebi integration restart <name> / stop <name>
trebi integration logs <name> -f
trebi integration remove <name>
```

Cursors track how far an integration has consumed:

```bash
trebi integration cursor show <name>
trebi integration cursor set <name> <last-event-id>
trebi integration cursor reset <name>
```

Test routing rules against a sample event before you put them in a job:

```bash
echo '{"channel":"ops","text":"deploy prod"}' | \
  trebi rules test --match "e.channel === 'ops'" --extract "({t: e.text})" --json

trebi rules fire --integration slack --job my-job --workspace <id> \
  --match "..." --extract "..." --event-file ./event.json
```

`rules test` only evaluates. `rules fire` evaluates and enqueues on a match.

Durable trigger events:

```bash
trebi events list --limit 50 --json
trebi events retrigger <id>        # re-inject a stored event
```

## Event bus

`trebi bus` is the live stream. It is not the trigger pipeline (`trebi events`) and it is not a complete audit trail — retention is 7 days.

```bash
trebi bus --replay --topics run --limit 10 --json   # last run events, then exit
trebi bus                                        # live frames (blocks)
trebi bus --topics run.failed,alert --json       # server-side topic filter
trebi bus --topics run --replay --limit 100      # include retained history
trebi bus --cursor <seq> --json                  # resume from a known point
trebi bus --entity run --id <id> --action updated  # client-side entity filter
```

`--topics` is server-side (family or exact topic). `--entity` is a client-side filter on the same frames. Prefer `--topics run` for "run events".

Frames are thin: `entity`, `id`, `action`, `revision`. They carry no payload — refetch with `trebi runs show` / `trebi job show` when you need the object.

`trebi bus` blocks. Use `--limit` so it exits, or run it in the background. Never follow with `-f` / no `--limit`.

## Skills

```bash
trebi skill list --json
trebi skill show <name>
trebi skill create <name> --file ./SKILL.md
cat SKILL.md | trebi skill update <name>
trebi skill file ls  <skill> [prefix] --json --limit
trebi skill file get <skill> helper.py
trebi skill file put <skill> helper.py --from ./helper.py
trebi skill file rm  <skill> helper.py
trebi skill delete <name>
```

Create the skill first (`trebi skill create`), then put scripts with `trebi skill file`. `SKILL.md` writes 409 here — use `trebi skill update`. File writes are denied for non-local provenance (git/registry/integration).

## Secrets

```bash
trebi secret list           # names only; values are never printed
trebi secret set <name> <value>
trebi secret delete <name>
```

Reference secrets from a job through `envset` or `env`, never inline in the prompt.

## Harnesses

A harness is the agent tool that runs the session (claude, codex, …).

```bash
trebi harness list --json
trebi harness list --check        # exit 1 when a home is missing
trebi harness ensure <name>       # create the harness home + lifecycle install
trebi harness auth status [name] [--live] [--json]
trebi harness auth login <name>              # remote device/paste flow
trebi harness auth login <name> --token-stdin
trebi harness auth login <name> --attach     # full TUI over WebSocket
trebi harness auth logout <name> [--yes]
trebi harness auth mode <name> shared|separate
```

Login runs on the daemon (tmux, harness isolation env) and is driven through the API. The CLI prints the verification URL + user code; complete OAuth in any browser and paste the code back. `--attach` opens the vendor TUI over WebSocket. Cursor has no device flow — use `--token-stdin` or log in on the daemon machine. On macOS, claude credentials live in the Keychain (file bridge cannot share them); `trebi harness auth login claude` runs `setup-token` in both shared and separate mode and injects `CLAUDE_CODE_OAUTH_TOKEN`. An API key via `--token-stdin` also works.

## Config

```bash
trebi config show                        # table: KEY VALUE SOURCE DEFAULT DESCRIPTION
trebi config show --json                 # array of {key,value,source,default,description}
trebi config show --yaml                 # same list as YAML
trebi config show --tree                 # nested tree, value/source/default leaves
trebi config show -k tools.grok          # section prefix
trebi config show -k defaults.command    # exact key
trebi config show -k 'defaults.*'        # glob (quote it)
trebi config path
trebi config set <dotted.key> <value>    # prints the resulting effective row
trebi config validate                    # works with the daemon down
trebi config bridge status [tool] --json
trebi config bridge set <tool> <resource> inherit|false
trebi config bridge set --global never <resource>
```

`show` lists every known key, not only the ones written to config.yaml. The table cuts VALUE at 40 characters; use `--json`, `--yaml`, or `--tree` to read a long value in full. `SOURCE` is `config` when the key is declared in the file and `default` when the built-in applies. Bare `trebi config` prints the help menu. When the daemon is up, `show` reports the server config, so a remote CLI sees the daemon truth and not its own local file.

## Conversations

```bash
trebi conversation list --limit 50 --json
trebi conversation show <id> --json
```

## Daemon lifecycle

```bash
trebi service install            # launchd LaunchAgent (macOS); --no-tray for headless
trebi service install --bin /usr/local/bin/trebi
trebi service start / stop / restart
trebi service status --json
trebi service logs -n 200 --level error --grep timeout
trebi service drain              # block automatic producers; manual launches still pass
trebi service undrain
trebi service shutdown           # ask the daemon to exit
trebi service migrate-db         # apply store migrations
trebi service token show         # the local bearer token
trebi service uninstall
```

`service logs` takes `--lines`/`-n` and `--grep`. Do not pass `--limit` (that is `bus` / list commands). Drop `-f` unless you intend to block. Do not run `trebi service daemon` from an agent session — that is the foreground dev entrypoint; use `start`/`stop`.

Drain returns 503 `draining` to automatic producers. Use it before maintenance, then `undrain`.

The daemon logs each API request as `api.request`. Successful reads of `/v1/system/health` and `/v1/agents/state` log at debug, because the macOS tray polls them every 2 seconds. Everything else, and any poll that fails, logs at info. Use `--log-level debug` to see the polls.

## Environment

| Variable | Meaning |
|---|---|
| `TREBI_HOME` | state directory; defaults to `~/.trebi`. Set it to sandbox a test daemon |
| `TREBI_WORKSPACE` | default workspace, same as `--workspace` |
| `TREBI_URL` | remote daemon base URL |
| `TREBI_TOKEN` | bearer token for `TREBI_URL` |
| `TREBI_BIN` | binary path used by `service install` |
| `TREBI_LOG_LEVEL` | daemon log level: `debug`, `info` (default), `warn`, `error` |

## Errors

Every error is `{"error":{"code","message","details"}}` with a stable code:

| Code | Meaning and what to do |
|---|---|
| `not_found` | wrong id or name. List first |
| `conflict` | the name already exists, or the state forbids it |
| `ambiguous` | the name matches several objects. Use the id |
| `validation` | bad input. Run `trebi job validate` |
| `draining` | the daemon is draining. Run `trebi service undrain` |
| `precondition_failed` | stale `If-Match` on a document. Re-read, then write |
| `unauthorized` | bad or missing token. Run `trebi service token show` |
| `internal` | server fault. Read `trebi service logs --level error` |

## Recipes

**Create and launch a job**

```bash
trebi workspace use my-repo
trebi job create daily-report --file ./JOB.md
trebi job validate daily-report
trebi job run daily-report --test        # confirm the plan
trebi job run daily-report
trebi job tail daily-report -n 200
```

**Generate a job and the script it runs (remote-safe)**

```bash
trebi workspace use my-repo
trebi job create nightly-report --file ./JOB.md
trebi job file put nightly-report render.py --from ./render.py
trebi job file put nightly-report email.html --from ./email.html
trebi job validate nightly-report
trebi job run nightly-report --test
```

**Debug a failed run**

```bash
trebi runs list --status failed --limit 10 --json
trebi runs show <id> --json | jq '.end_reason, .duration_ms, .cost_usd'
trebi runs timeline <id>
trebi service logs --level error -n 100
trebi runs retry <id>
```

**Watch a job until it finishes**

```bash
trebi job run my-job --json
# poll; do not kill and re-prompt
trebi runs show <id> --json | jq '{id,status,end_reason}'
trebi bus --replay --topics run --limit 20 --json
trebi runs list --job my-job --limit 1 --json | jq '.runs[0].status'
```

For the assistant, poll `trebi conversation messages <id> --json` the same way. Do not `runs kill` a live session to "unstick" it.

**Wire an integration to a job**

```bash
trebi integration set slack --type stream --format json -- ./slack-stream
trebi integration test slack
echo '{"channel":"ops","text":"deploy"}' | trebi rules test \
  --match "e.channel === 'ops' && /deploy/.test(e.text)" --json
# add the trigger to JOB.md, then:
trebi job validate my-job
trebi events list --limit 20
```

**Daily triage**

```bash
trebi service status --json
trebi doctor --json
trebi inbox list --status unread --json
trebi runs approvals --json
trebi runs list --status failed --limit 20
```

## Rules for the agent

- Prefer `--json` plus `jq` over parsing tables.
- Run `trebi job validate` after every `JOB.md` write.
- Use `trebi job run --test` before the first real launch of a new or edited job. Not `rules fire`, not `assistant say`.
- Ask the user before `job delete` / `job delete --purge`, `workspace remove`, `memory clear`, `secret delete`, `integration remove`, and `service uninstall`. They destroy state. `runs kill` of a run you just started to debug is not in that list.
- Do not write to `~/.trebi` / `$TREBI_HOME` and do not call `/v1` with `curl`. The daemon is the single writer. If `trebi doctor` is healthy, you are done.
- Never `trebi service daemon`, never `kill` trebid. `service start` / `stop` / `shutdown` only.
- `trebi bus` and `integration logs` block without `--limit` (or with `-f`). `service logs` uses `--lines`/`-n`, not `--limit`; drop `-f` unless you intend to block.
- Put `-w` / `--workspace` before the verb or before the job name. Flags after a positional argument are ignored/rejected.
