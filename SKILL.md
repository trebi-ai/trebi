---
name: trebi
description: Operate trebi, an LLM task runner (daemon + CLI), fully from the shell. Use when the user wants to create, run, schedule, or debug trebi jobs; put supporting files next to a job; inspect or attach to runs; manage workspaces, agents, skills, secrets, connectors, and event rules; drain the inbox or alerts; tail the event bus; link the daemon to a Trebi cloud account; mint hosted webhooks for job triggers; or control the trebid daemon. Triggers: "trebi job", "run a trebi job", "schedule a job", "trebi job file", "put a script next to a job", "trebi file", "trebi runs", "attach to a run", "trebi daemon", "trebi workspace", "trebi agent", "wake an agent", "trebi inbox", "trebi bus", "trebi connector", "trebi doctor", "wake the assistant", "trebi assistant say", "trebi assistant chat", "trebi conversation", "trebi cloud link", "trebi hooks", "hosted webhook".
---

# Trebi

Trebi is one binary that is both a daemon (`trebid`) and a CLI. The daemon owns an HTTP `/v1` API, an SSE event bus, and a SQLite store under `~/.trebi`. The CLI is a thin client of that API and covers the whole control plane; the MCP surface and the native assistant share the same 16 catalog tools.

## How to reach trebi

If this session has trebi tools registered (`run`, `job`, `workspace`, `file`, `system`, and the rest of the catalog), use those. Call `docs` on a tool before its first use. Do not shell out to `trebi` for anything the catalog covers. `bash` is for the workspace filesystem: read a log, run a build, grep.

If this session has no trebi tools, drive trebi only through the `trebi` CLI. Do not edit files under `~/.trebi` and do not call the API with `curl`.

## Object model

| Object | What it is |
|---|---|
| workspace | a registered folder; the unit of context. Jobs, runs, and the assistant belong to one |
| job | a `JOB.md` file: YAML frontmatter (harness, triggers, session) plus a prompt body |
| file | any path inside a workspace (scripts, templates, eval files). Raw bytes; no job semantics. Job-scoped via `trebi job file`; workspace-wide via `trebi file` |
| run | one launch of a job in a harness session (subprocess by default; tmux when `runtime: tui`) |
| assistant | the workspace chat agent; one live session per conversation, settings in config and `trebi.yaml` |
| agent | a folder-scoped `AGENT.md` at `agents/<name>/`; owns the jobs inside that folder, kind=agent |
| conversation | a chat thread with the assistant; the CLI/UI are channels onto it |
| connector | one external system with up to three capabilities: actions (an MCP server or CLI commands for agent runs), events (a supervised process that produces trigger events), and a channel (a conversation transport) |
| trigger | a rule on a job: `cron`, `every`, or `event` from a connector |
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

Only these work with the daemon down: `service install|start|stop|status`, `doctor`, `config validate|path|show|set|unset`, `guide`, `mcp config|install`, `memory validate|skeleton`, and `workspace list|current|use|unlink`. Everything else fails with a clear message.

Native assistant (no vendor CLI): `trebi config set assistant.native.model claude-sonnet-4-5` and `trebi config set assistant.native.api_key_env ANTHROPIC_API_KEY`. API key only.

## Cloud link and hosted webhooks

`trebi cloud link --code <code>` links this daemon to a Trebi cloud account; the account page mints the code and it expires in 10 minutes. `trebi cloud status` shows the plan, the granted features, and the socket state. The plan gates hosted mail and hosted webhooks in the cloud, not on this machine: `trebi cloud unlink` only drops the local link.

```bash
trebi cloud status --json
trebi hooks create --workspace reports --label deploy
trebi hooks list
```

`hooks create` prints the public URL once. Handle it like a webhook secret: anyone holding it can post an event. A job fires on a delivery with `on: [{event: webhook, match: …}]`; the event carries `path`, `headers`, `body` as a string, and the delivery facts. Delete a hook with `trebi hooks delete <id>`.

Vendor runs (`claude`, `codex`, `opencode`, `opencode2`, `pi`, `copilot`, `cursor`) spawn the installed CLI as a subprocess and speak its structured protocol by default — no tmux pane, no prompt pasting. `runtime: tui` in `JOB.md` / `AGENT.md` opts that run back into a pane; `trebi config set harness.runtime tui` is the incident-only global kill switch. A CLI run that fails closed leaves a doctor hint; relaunch or resume with `runtime: tui` — there is no silent fallback. `trebi runs attach` on a CLI run prints the event stream; `trebi runs logs -f` always works.

## Global rules

- `--workspace` / `-w` (or `$TREBI_WORKSPACE`) selects the workspace by **name or id**. Flags can go before or after the name: `trebi -w staging job run report`, `trebi job run --workspace staging report`, and `trebi job run report -w staging` are equivalent.
- Without `-w`, trebi uses the sticky context from `trebi workspace use`. On `not_found` after `-w <name>`, run `trebi workspace list --json` and retry with the `id`.
- List / show / status / current commands take `--json`. `runs show`, `job show`, `skill show`, and `assistant show` already print JSON; `--json` is accepted and is a no-op. `job validate --json` emits the validate body. Mutating commands (`create`, `set`, `patch`) and `assistant say` often do not. On `flag provided but not defined: -json`, drop `--json` once; do not retry it.
- Use `--json` and pipe to `jq` when you parse output. Never parse the human tables.
- List commands paginate with `--limit` and `--cursor`. The cursor is the id of the last row. `trebi service logs` is not a list command: it uses `--lines` / `-n` and `--grep`, not `--limit`.
- Times are RFC3339 UTC. Durations end in `_ms`. Keys are snake_case.
- The server computes `duration_ms`, `cost_usd`, and `billed`. Never recompute money or duration yourself.

## Guides

Run `trebi guide <topic>` before starting a workflow you have not done in this session. `trebi guide` lists topics.

| Topic | Covers |
|---|---|
| `getting-started` | fresh install → workspace → first job → first run |
| `jobs-and-scheduling` | JOB.md shape, cron/every/event triggers, pause, validate, job memory |
| `runs-and-debugging` | show/timeline/transcript/logs, retry/resume/replay/mark, attach, approvals |
| `files` | job-scoped vs workspace files, scripts next to jobs, If-Match |
| `assistant` | settings layers, chat and sessions, conversations, memory |
| `agents` | folder-scoped agents: identity, rename, email accept, owned jobs, wake, conversations, agent memory |
| `connectors-and-rules` | create/connect connectors, registry search/entry/install, the job `connectors:` key (MCP and CLI actions for a run), rules test/fire, event triggers, queue |
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
| `unauthorized` | the daemon refused the call. The local API is open, so this comes from a remote or hosted service |
| `internal` | server fault. Read `trebi service logs --level error` |

## Rules for the agent

- If trebi tools are registered, use them instead of the CLI recipes below. The same rules apply: validate after a JOB.md write (`job.validate`), dry-run a job you wrote (`job.run` with `dry_run`), dry-run an agent you wrote (`agent.wake` with `dry_run`), ask before destructive.
- Prefer `--json` plus `jq` over parsing tables. If a command rejects `--json`, drop it once. `runs show --json` is valid.
- Run `trebi job validate` after every `JOB.md` write.
- Memory (`job` / `agent` / `assistant memory …`) is a structured `MEMORY.md`: Context, State, Lessons, Log. Edit it through the section commands (`set-context`, `set-state`, `lesson add`, `log add`), not by rewriting the whole file with `set`. Lessons persist; log entries are capped and pruned oldest-first. A run that ends with an invalid file gets two fix injections, then a `memory_invalid` alert.
- Use `trebi job run --test` (or `trebi agent wake --test`) before the first real launch only when **this session** created or edited that `JOB.md` / `AGENT.md`. Not `rules fire`, not `assistant say`, not a job that already existed.
- Ask the user before `job delete` / `job delete --purge`, `agent delete` (plain or `--purge`), `workspace delete`, `workspace move`, `memory clear`, `agent memory clear`, `assistant memory clear`, `secret delete`, `connector delete`, `connector install`, `cloud unlink`, `hooks delete`, and `service uninstall`. They destroy state (move rewrites files; install runs third-party code). Ask **before** you make the call, not after: state what the action deletes, then stop and wait for the answer. Do not send the call to see what it says. The daemon refuses it without `--force` (`confirm=1` over the API) and names what it deletes, but that gate is a backstop, not your confirmation step. `runs kill` of a run you just started to debug is not in that list. `--dry-run` on `workspace move` is not in that list.
- If a trebi command fails with "Confirm with the user first", stop. That refusal is the confirmation gate. Report the consequence to the user and wait. Do not retry with `--force` in the same turn.
- Never show a secret value you did not have to read. Always prefer the form that keeps it out of your transcript: on the CLI pass the path, `trebi secret set --from-file ./key.txt NAME`; on the catalog pass `value_file`, which is resolved before dispatch. If you only have the value in context, pass `value` once and never show it again: no second `cat`, not in the final message, not in a job prompt or a file. Do not stop the task over this — store the secret and carry on. Reference a stored secret from a job by its **name**; the daemon resolves it at launch. A secret name is not a secret.
- Do not write to `~/.trebi` / `$TREBI_HOME` and do not call `/v1` with `curl`. The daemon is the single writer. If `trebi doctor` is healthy, you are done.
- Agent runs carry `TREBI_RUN_TOKEN`; keep it in the environment for `trebi` and `trebi mcp serve` calls. It scopes an agent to its folder, owned jobs, and own memory. `email.accept`, `kit`, and `budget` are human-only. If a write returns `forbidden`, ask the human instead of changing owners or protected keys.
- To hand a problem up, use `agent escalate` (`to=assistant` or `to=operator`) with the concrete blocker and what you tried. A failed owned job first wakes its owner with `reason=escalation`; the fourth failure in 24 hours goes to the operator inbox.
- Never `trebi service daemon`, never `kill` trebid. `service start` / `stop` / `shutdown` only.
- `trebi bus --limit N` dumps up to N frames and exits. `connector logs` returns at once; `-f` follows and blocks. `service logs` uses `--lines`/`-n`, not `--limit`; drop `-f` unless you intend to block.
- `-w` / `--workspace` accepts name or id and can go before the verb or after the job name.
- Do not `runs attach` from an agent session. Poll `runs show` / `conversation messages`. Do not `runs kill` a live session to re-prompt.
- Report only what a call returned. Do not carry a value from one call into a claim about another: if a filtered read returned nothing, say it returned nothing, and do not borrow a count or a cursor from the unfiltered read next to it. If a read failed or came back `not_found`, say so — do not claim what you would have seen. Reporting a failure plainly is a good answer; an unsupported claim is not.
