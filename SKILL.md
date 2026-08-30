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

Only these work with the daemon down: `service install|start|stop|status`, `doctor`, `config validate`, `guide`. Everything else fails with a clear message.

## Global rules

- `--workspace` / `-w` (or `$TREBI_WORKSPACE`) selects the workspace by **name or id**. Flags can go before or after the name: `trebi -w staging job run report`, `trebi job run --workspace staging report`, and `trebi job run report -w staging` are equivalent.
- Without `-w`, trebi uses the sticky context from `trebi workspace use`. On `not_found` after `-w <name>`, run `trebi workspace list --json` and retry with the `id`.
- List / show / status / current commands take `--json`. `runs show`, `job show`, `skill show`, and `assistant show` already print JSON; `--json` is accepted and is a no-op. `job validate --json` emits the validate body. Mutating commands (`create`, `add`, `set`) and `assistant say` / `wake` often do not. On `flag provided but not defined: -json`, drop `--json` once; do not retry it.
- Use `--json` and pipe to `jq` when you parse output. Never parse the human tables.
- List commands paginate with `--limit` and `--cursor`. The cursor is the id of the last row. `trebi service logs` is not a list command: it uses `--lines` / `-n` and `--grep`, not `--limit`.
- Times are RFC3339 UTC. Durations end in `_ms`. Keys are snake_case.
- The server computes `duration_ms`, `cost_usd`, and `billed`. Never recompute money or duration yourself.

## Guides

Run `trebi guide <topic>` before starting a workflow you have not done in this session. `trebi guide` lists topics.

| Topic | Covers |
|---|---|
| `getting-started` | fresh install → workspace → first job → first run |
| `jobs-and-scheduling` | JOB.md shape, cron/every/event triggers, pause, validate |
| `runs-and-debugging` | show/timeline/transcript/log, retry/resume/replay, attach, approvals |
| `files` | job-scoped vs workspace files, scripts next to jobs, If-Match |
| `assistant` | wake, say, conversations, memory |
| `integrations-and-rules` | create/connect integrations, rules test/fire, event triggers, queue |
| `inbox-and-alerts` | draining the inbox, approvals, the alert feed |
| `observability` | bus, service logs, doctor, stats |
| `skills-secrets-config` | stored skills, secrets, config keys, harness auth |
| `mcp-basics` | remote MCP entry point, CLI→MCP translation, what MCP cannot do |

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

## Rules for the agent

- Prefer `--json` plus `jq` over parsing tables. If a command rejects `--json`, drop it once. `runs show --json` is valid.
- Run `trebi job validate` after every `JOB.md` write.
- Use `trebi job run --test` before the first real launch only when **this session** created or edited the `JOB.md`. Not `rules fire`, not `assistant say`, not a job that already existed.
- Ask the user before `job delete` / `job delete --purge`, `workspace remove`, `memory clear`, `secret delete`, `integration remove`, and `service uninstall`. They destroy state. `runs kill` of a run you just started to debug is not in that list. `--force` on `workspace remove` skips the CLI confirm; you still ask the user first.
- If a trebi command fails with "Confirm with the user first", stop. That refusal is the confirmation gate. Report the consequence to the user and wait. Do not retry with `--force` in the same turn.
- Do not write to `~/.trebi` / `$TREBI_HOME` and do not call `/v1` with `curl`. The daemon is the single writer. If `trebi doctor` is healthy, you are done.
- Never `trebi service daemon`, never `kill` trebid. `service start` / `stop` / `shutdown` only.
- `trebi bus --limit N` dumps up to N frames and exits. `integration logs` blocks without `--limit` (or with `-f`). `service logs` uses `--lines`/`-n`, not `--limit`; drop `-f` unless you intend to block.
- `-w` / `--workspace` accepts name or id and can go before the verb or after the job name.
- Do not `runs attach` from an agent session. Poll `runs show` / `conversation messages`. Do not `runs kill` a live session to re-prompt.
