---
name: trebi
description: Operate trebi, an LLM task runner (daemon + CLI), fully from the shell. Use when the user wants to create, run, schedule, or debug trebi jobs; put supporting files next to a job; inspect or attach to runs; manage workspaces, agents, skills, secrets, connectors, connections, and event rules; find why a trigger did not fire; send email with HTML and attachments; drain the inbox or alerts; tail the event bus; link the daemon to a Trebi cloud account; mint hosted webhooks for job triggers; or control the trebid daemon. Triggers: "trebi job", "run a trebi job", "schedule a job", "trebi job file", "put a script next to a job", "trebi file", "trebi runs", "attach to a run", "trebi daemon", "trebi workspace", "trebi agent", "wake an agent", "trebi inbox", "trebi outbox", "trebi notify", "alert route", "trebi config schema", "validate a config change", "trebi bus", "trebi connector", "add a connection", "add a connector from GitHub", "connector login", "why did my job not fire", "rules test", "run turns", "trebi email send", "held mail", "release a reply", "trebi decisions", "trebi artifact", "save a report", "send a draft", "live artifact", "refresh a dashboard", "trebi status", "trebi doctor", "wake the assistant", "trebi assistant say", "trebi assistant chat", "trebi conversation", "trebi cloud link", "trebi hooks", "hosted webhook", "trebi mobile", "pair a phone".
---

# Trebi

Trebi is one binary that is both a daemon (`trebid`) and a CLI. The daemon owns an HTTP `/v1` API, an SSE event bus, and a SQLite store under `~/.trebi`. The CLI is a thin client of that API and covers the whole control plane; the MCP surface and the native assistant share the same catalog tools.

## How to reach trebi

If this session has trebi tools registered (`run`, `job`, `workspace`, `file`, `system`, and the rest of the catalog), use those. Call `docs` on a tool before its first use. Do not shell out to `trebi` for anything the catalog covers. `bash` is for the workspace filesystem: read a log, run a build, grep.

If this session has no trebi tools, drive trebi only through the `trebi` CLI. Do not edit files under `~/.trebi` and do not call the API with `curl`.

## Object model

| Object | What it is |
|---|---|
| workspace | a registered folder; the unit of context. Jobs, runs, and the assistant belong to one |
| machine | one install of Trebi; each workspace is on one machine (`machine` on the workspace). `trebi machine ls` lists them; `trebi machine show [id\|self]` gives the harnesses and the connections of this machine with a state each. `job show` and `agent show` give `readiness`: one item per harness, connector, and secret that the job needs, with its `state` and the `action` that fixes it (`Log in`, `Install`, `Connect`, `Add secret`). `trebi machine sessions` lists the agent sessions that the user started outside Trebi on this machine, and `trebi machine session <id>` gives the turns of one; Trebi sees them only after `trebi harness hooks <name> --on`. `trebi machine connect <id> <link>` connects to another machine of the account with the pairing link that the other machine made (`trebi mobile pair` there); then the UI can open that machine. `trebi machine disconnect <id> --force` removes the link |
| job | a `JOB.md` file: YAML frontmatter (harness, triggers, session) plus a prompt body. The first frontmatter key is `id:` (`jb_…`; an agent has `agt_…`). The daemon writes it; do not change or copy it. Commands take the id or the name |
| state | the records of a job or an agent: rows with a key, an optional time, and a JSON value, in named collections. Declare collections with `state:` in the frontmatter (description, ttl, schema). Each run gets a `## State` brief. `trebi state [--owner <id|name>]` shows a summary; `query|get|has|put|delete|prune|drop <collection>` read and write rows; in a run the owner is `self` (`trebi guide jobs-and-scheduling`) |
| file | any path inside a workspace (scripts, templates, eval files). Raw bytes; no job semantics. Job-scoped via `trebi job file`; workspace-wide via `trebi file` |
| run | one harness session of a job (subprocess by default; a terminal pane when `runtime: tui`). Resume reopens the same run as a new turn: the id stays and `turn_count` goes up. Cost and `duration_ms` sum all turns. `previous_run_id` names the run that a rerun follows. `trebi runs turns <id>` lists the turns |
| assistant | the workspace chat agent; one live session per conversation, settings in config and `trebi.yaml` |
| agent | a folder-scoped `AGENT.md` at `agents/<name>/`; owns the jobs inside that folder, kind=agent. An owned job can set `folder: <name>` to group it under the agent (one level, no `/`; `job patch <name> folder=<name>`). An owned job with no `working_dir` runs in the `working_dir` of its agent; `job show` gives `working_dir` and `working_dir_source` (`job`, `agent`, `default`) |
| conversation | a chat thread with the assistant; the CLI/UI are channels onto it |
| connector | one external system with up to three capabilities: actions (an MCP server or CLI commands for agent runs), events (a supervised process that produces trigger events), and a channel (a conversation transport). A type from the catalog (for example `whatsapp`) can have many **connections**, one per account. Each connection has an id (`con_…`), a slug, and a title; jobs, triggers, and notify steps use the slug. Its `scope` is `global` (every workspace) or a list of workspaces; a connector with an account gets the current workspace when you add it. `trebi connector scope <slug> [global|ws1,ws2]` shows or sets it |
| email | the native channel. Each workspace has an address `<handle>-<token>@<domain>` (`trebi workspace email show`); agents send from a subaddress. `trebi email send --text` is Markdown, and Trebi renders the HTML part in the workspace style (`email.style`; report blocks in `trebi guide email-format`). `--html` or `--html-file` sends your own HTML instead, and Trebi makes the text part from it. Give `--text` or the HTML, never both. `--attach` and `--workspace-file` add files (10 files, 5 MiB total). Put `email` in an agent `kit.connectors` (or a job `connectors:`): each run then gets the send command in its prompt and `TREBI_WORKSPACE`/`TREBI_AGENT` in its env, so `--workspace` and `--agent` drop out. An owned job sends as its owner agent. A job sends only to admins, the workspace email allowlist (`trebi workspace set --email-allowlist`), the `allowlist` of its own `event: email` triggers, and the addresses the owner agent `email.accept` accepts. A reply in a thread that an agent started reaches that agent when the sender is on the thread and the safety check passes; else it waits as held mail (`trebi email held`). Only the user releases or ignores held mail (`trebi email release <id> --scope once|thread|sender|domain`, `trebi email ignore <id>`) |
| decision | a safety check that Trebi makes before it acts, for example on an email reply (kind `email.reply_risk`). `trebi decisions ls`, `show <id>`, `kinds`, `stats`, `test <kind> --text <file>`, and `feedback <id> agree\|disagree`. A check that does not finish holds the mail |
| artifact | a named, versioned document of a workspace: a report, a table, or an email draft. `trebi artifact put <name> --from <file>` saves a new version; a change starts from `trebi artifact get <name>` and saves with `--base N`. `trebi artifact send <name>` sends an email draft from its `from:` line; `trebi email send --artifact <name>` mails a report. The JOB.md `artifacts:` key saves files at the end of a run. A live artifact adds `--sources <file.yaml>` (read-only connection operations); `trebi artifact refresh <name>` stores a new snapshot. `trebi artifact chat <name>` gives the one chat of the artifact with its owner; `trebi artifact ask <name> <text>` sends a request into that chat |
| trigger | a rule on a job: `cron`, `every`, or `event` from a connector. An event trigger filters with `rooms`, `allowlist`, `from` (people, bots, anyone), `contains` (words in the message), and optional `match`; on an `event: email` trigger the `allowlist` also names a send address of the job; `replies: continue` sends later messages of a conversation to the same run; `batch: 15m` collects the events and starts one run per interval with all of them (`trebi guide jobs-and-scheduling`) |
| skill | a stored `SKILL.md` the harness can load; global, or for one workspace, agent, or job |
| request | a batch of forms that a job run asks a person to answer (confirm, review, choose, form). The create calls of one turn join one request. The run ends its turn as `parked` and continues with the answers |
| inbox | the queue you drain: approvals, requests, failures, health, watchdog, escalations. Read, unread, archive to Done |
| alerts | the per-firing alert feed; a log, not a queue |
| outbox | every outgoing message (channel reply, email, alert mail) with its status, attempts, and last error |
| notify route | the ordered steps an alert tries (email, then connector channels); set per job, agent, workspace, or globally. `trebi notify route` shows it, `trebi notify test` sends a test |
| queue | the dispatch queue of pending launches. With `session.if_running: queue` (the default), a new launch waits behind the live run of the same job; `skip` drops it, `replace` stops the live run |
| proposal | an agent change to its `AGENT.md` that a person approves or denies (`trebi proposals`). Human workflow: do not decide one from an agent run |
| bus | the live event stream (`trebi bus`) |

## First checks

Run these before anything else. Most failures are a daemon that is down.

```bash
trebi service status --json      # OS service + API health
trebi status --json              # one screen: health, live runs, inbox, next due
trebi doctor --json              # findings; add --fix to repair automatically
trebi workspace current --json   # the sticky workspace context
```

If `doctor` is healthy, stop. Do not "fix" the database, edit `$TREBI_HOME`, or send signals to trebid.

If the daemon is down: `trebi service start`. If it was never installed: `trebi service install` then `trebi service start`. Never run `trebi service daemon` yourself and never `kill` the daemon PID. `service daemon` is a foreground dev entrypoint; `start`/`stop` own the process.

Only these work with the daemon down: `service install|start|stop|status`, `doctor`, `config validate|path|show|set|unset`, `guide`, `mcp config|install` (not `mcp config --remote`), `memory validate|skeleton`, and `workspace list|current|use|unlink`. Everything else fails with a clear message.

Native assistant (no vendor CLI): `trebi config set assistant.native.model claude-sonnet-4-5` and `trebi config set assistant.native.api_key_env ANTHROPIC_API_KEY`. API key only.

## Troubleshooting map

Start from the symptom. Each line is a read first, then the fix. Read the matching guide for the details.

| Symptom | Read | Then |
|---|---|---|
| a run failed or hangs | `runs show <id> --json`, `runs turns <id> --json`, `runs timeline <id> --json`, `runs logs <id>`, `service logs --level error --lines 100 --grep <id>` | `runs retry <id>` (it picks resume, park, or rerun). Do not kill a live run to re-prompt |
| a job did not start on schedule | `job status <job> --json`, `queue --json`, `inbox ls --json` | a launch can wait behind a live run (`if_running: queue`). `job unpause` if paused |
| a job did not start on an event | `connector events <slug>` (a skipped type is not stored), `event list --connector <slug> --since 1h --json`, then `rules test --event-id <id> --job <job> --json` with the trigger keys | `stage` names the check that stopped it: `type`, `room`, `sender`, `from`, `contains`, `extract`, `match`, or `inputs`. A `manual: true` trigger never starts a run by itself: `event jobs <id>` lists the jobs that can run with the event, and `event run <id> --job <job>` starts one. `connector senders` gives allowlist values. The trigger `limit` (default `15/10m`) holds extra events in order; at 10 waiting, new events drop and an inbox item opens. `event retrigger <id>` after the fix |
| a connector is down or silent | `connector show <slug> --json`, `connector logs <slug>`, `connector dependents <slug>` | `connector restart`, `connector login-methods`, then `connector login [--method <id>]` (ask the user to follow the steps; for a key, give the user the login form link), `connector check-updates` |
| a watched thing gives no events | `connector watching <slug> --json`, `doctor --json` | `action_required`: tell the user the steps, then `connector watch-finish <slug> <id> --set name=value`. `error`: fix the cause, then `connector watching <slug> --sync`. Live updates need a linked Trebi Cloud account |
| a run failed with `connector_login`, or an online server asks for a login | `connector oauth show <slug> --json`, `doctor --json` (`connector_login_required`) | `connector login <slug>` (ask the user to finish it in the browser) |
| a GitHub link does not install | `connector check-link <link> --json`: read each `fail` and `warn` row | fix the folder in its repository (the connectors guide, "Make a folder a connector"), push, then check the link again. `not_found` on a private repository: the GitHub login of this computer cannot read it |
| an alert or reply did not arrive | `notify route --json`, `outbox ls --json`, `inbox show <id> --json` | `outbox retry <id>`, `notify test` |
| email does not arrive | `email status --json`, `email messages --direction in --json`, `email show <id> --json`, `email held --json` | `email poll` pulls the spool now; a held reply waits for the user, and `decisions show <decision_id>` gives the reasons |
| a job cannot run on this machine | `job show <job> --json` (`readiness`), `machine show --json` | fix each item that is not `ready` with its `action`: `harness auth login`, `connector add` or `connector login`, `secret set` |
| a harness cannot log in | `harness auth status --json`, `doctor --json` | `harness auth login` (ask the user) |
| a harness program is missing or old | `harness list --json` (`installed`, `version`, `last_update`) | `harness install <name> [--version]` or `harness update <name>` (ask the user); agent-visor installs its own copy and keeps the user's copy as it is |

## Cloud link and hosted webhooks

`trebi cloud link --code <code>` links this daemon to a Trebi cloud account; the account page mints the code and it expires in 10 minutes. `trebi cloud status` shows the plan, the granted features, and the socket state. The plan gates hosted mail and hosted webhooks in the cloud, not on this machine: `trebi cloud unlink` only drops the local link.

```bash
trebi cloud status --json
trebi hooks create --workspace reports --label deploy
trebi hooks list
```

`hooks create` prints the public URL once. Handle it like a webhook secret: anyone holding it can post an event. A job fires on a delivery with `on: [{event: webhook, match: …}]`; the event carries `path`, `headers`, `body` as a string, and the delivery facts. Delete a hook with `trebi hooks delete <id>`.

### Phones and tablets

The Trebi app on a phone or a tablet uses this machine through Trebi Cloud. It needs a linked machine, the Pro Cloud plan, and `cloud.mobile` on (the default). `trebi mobile pair` shows a QR code and a link, then waits until the phone pairs. The link holds a secret: do not share it. A phone with the `read` scope can only look.

```bash
trebi mobile pair --name "Work phone" --ttl 30m
trebi mobile ls --json
trebi mobile scope <id> read
trebi mobile rename <id> "iPad"
trebi mobile rm <id> --force
```

`mobile rm` removes the phone here and in the cloud; the phone must pair again. `mobile cancel <pairing-id>` closes an open link. A phone cannot shut down the daemon, change the cloud link, or pair another phone.

Vendor runs (`claude`, `codex`, `opencode`, `opencode2`, `pi`, `copilot`, `cursor`) spawn the installed CLI as a subprocess and speak its structured protocol by default — no terminal pane, no prompt pasting. `runtime: tui` in `JOB.md` / `AGENT.md` opts that run back into a pane; `trebi config set harness.runtime tui` is the incident-only global kill switch. A CLI run that fails closed leaves a doctor hint; relaunch or resume with `runtime: tui` — there is no silent fallback. `trebi runs attach` on a CLI run prints the event stream; `trebi runs watch <id|job>` prints it without input and exits with the run result; `trebi runs logs -f` always works.

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
| `jobs-and-scheduling` | JOB.md shape, cron/every/event triggers, pause, validate, rename, move to another agent or folder (`job move`, `agent folder rename`), job memory, job state (`trebi state`, `state:` frontmatter) |
| `runs-and-debugging` | show/turns/timeline/transcript/logs, retry/resume/replay/mark, attach, approvals |
| `files` | job-scoped vs workspace files, scripts next to jobs, If-Match |
| `assistant` | settings layers, chat and sessions, conversations, memory |
| `agents` | folder-scoped agents: identity, rename, email accept, owned jobs, wake, ask and hand off (`agent ask`, `agent handoff`, `trebi chain`), conversations, agent memory, agent state |
| `connectors-and-rules` | add connections, login, rooms, senders, choose the events (`connector events`), bind, scope, overrides, versions and updates, registry search/entry/install, add from a GitHub link (`connector check-link <link>`, `connector add <link>`), add an online MCP server with its login (`connector add <https address>`, `connector login`, `connector oauth show|set`), packages with no connection for programs such as perception (`connector package install|list|keep`), make a folder a connector, the job `connectors:` key (MCP and CLI actions for a run), rules test/test-batch/fire, event triggers, retrigger, queue |
| `artifacts` | save, version, read, and send artifacts; email drafts; the JOB.md `artifacts:` key; the chat of an artifact (`chat`, `ask`); live artifacts (sources, refresh, snapshots, approve, schedule); connection operations (`trebi connector call`) |
| `email-format` | Markdown that Trebi mails as styled HTML: alerts, `kpi`/`status`/`button` report blocks, layouts, `email.style` |
| `inbox-and-alerts` | draining the inbox, approvals, the alert feed, alert routes (`trebi notify route|test`), failed sends in the outbox |
| `requests` | a job asks a person one or more forms (confirm, review, choose, form), ends its turn, and continues with the answers; answer and dismiss as a person |
| `observability` | bus, service logs, doctor, stats |
| `skills-secrets-config` | stored skills and their scopes (`skill --scope --owner`, `skill list --effective-for`, `skill move`, `skill approve`), secrets, config keys, config schema and draft checks, harness auth |
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
- Memory (`job` / `agent` / `assistant memory …`) is a structured `MEMORY.md`: Context, Lessons, Log. Keep records in `trebi state`, not in memory. Edit it through the section commands (`set-context`, `lesson add`, `log add`), not by rewriting the whole file with `set`. Lessons persist; log entries are capped and pruned oldest-first. A run that ends with an invalid file gets two fix injections. A damaged file then raises a `memory_invalid` alert; a file that only missed its Log entry does not. A later run with a sound file closes the alert.
- Use `trebi job run --test` (or `trebi agent wake --test`) before the first real launch only when **this session** created or edited that `JOB.md` / `AGENT.md`. Not `rules fire`, not `assistant say`, not a job that already existed.
- Ask the user before `job delete` / `job delete --purge`, `agent delete` (plain or `--purge`), `workspace delete`, `workspace move`, `memory clear`, `agent memory clear`, `assistant memory clear`, `secret delete`, `connector delete`, `connector install`, `connector add <link>` and `connector package install <link>` (Trebi did not review the code of a link; pass `--yes` only after the user agrees), `connector logout`, `workspace email rotate`, `cloud unlink`, `hooks delete`, `file rmdir` (and `job file rmdir`, `skill file rmdir`), `state delete|prune|drop` (outside a run; `--yes`), and `service uninstall`. They destroy state (move rewrites files; install runs third-party code). Ask **before** you make the call, not after: state what the action deletes, then stop and wait for the answer. Do not send the call to see what it says. For most of them the daemon refuses the call without `--force` (`confirm=1` over the API; `--yes` on `workspace email rotate`) and names what it deletes. `connector logout` has no gate. A gate is a backstop, not your confirmation step. `runs kill` of a run you just started to debug is not in that list. `--dry-run` on `workspace move` is not in that list.
- If a trebi command fails with "Confirm with the user first", stop. That refusal is the confirmation gate. Report the consequence to the user and wait. Do not retry with `--force` in the same turn.
- Never show a secret value you did not have to read. Always prefer the form that keeps it out of your transcript: on the CLI pass the path, `trebi secret set --from-file ./key.txt NAME`; on the catalog pass `value_file`, which is resolved before dispatch. If you only have the value in context, pass `value` once and never show it again: no second `cat`, not in the final message, not in a job prompt or a file. Do not stop the task over this — store the secret and carry on. Reference a stored secret from a job by its **name**; the daemon resolves it at launch. A secret name is not a secret. A job or agent run also loads `.env` from its working dir (multi-line quoted values work, no `${VAR}` expansion); `job show` gives `folder_env` with key names and skipped lines, never values. Env sets and inline `env` win over it. `job patch <name> env_file=false` turns it off.
- Do not write to `~/.trebi` / `$TREBI_HOME` and do not call `/v1` with `curl`. The daemon is the single writer. If `trebi doctor` is healthy, you are done.
- Agent runs carry `TREBI_RUN_TOKEN`; keep it in the environment for `trebi` and `trebi mcp serve` calls. It scopes an agent to its folder, owned jobs, and own memory. `email.accept`, `kit`, and `budget` are human-only. If a write returns `forbidden`, ask the human instead of changing owners or protected keys.
- When a job run needs a person's decision before an action, use `trebi request create` (or the `request` tool) and then end the turn. Ask all forms of the step in the same turn. Do not wait or poll. Do not answer a request from a job run; a person answers it. Read `trebi guide requests` first.
- To get an answer from another agent, use `agent ask <name> <text>`, then end your turn. The reply starts your next turn. Do not poll. To give work away with no reply, use `agent handoff`. Find the agent with `agent list --for <words>`. If an ask returns `chain_cycle`, the target waits for you: reply instead.
- To hand a problem up, use `agent escalate` (`to=assistant` or `to=operator`) with the concrete blocker and what you tried. A failed owned job first wakes its owner with `reason=escalation`; the fourth failure in 24 hours goes to the operator inbox.
- Never `trebi service daemon`, never `kill` trebid. `service start` / `stop` / `shutdown` only.
- Typed inputs: `job run <job> --input name=value` (repeatable). `job run <job> --event <id> [--trigger N] --test` fills the inputs from a stored event and shows each `{value, source}`; 422 `invalid` gives `details.fields`, 422 `event_not_accepted` means that no event trigger of the job takes the event. `job simulate <job> [--file JOB.md] [--event <id>… | --since 24h] [--preview]` tests a job or a draft against stored events and starts no run. `event list --outcome manual` lists the events that wait for a person.
- `trebi event list --after <id> --json` reads trigger events forward, oldest first; call it again with `next_after`. `--follow` blocks; do not use it from an agent session.
- `trebi bus --limit N` dumps up to N frames and exits. `connector logs` returns at once; `-f` follows and blocks. `service logs` uses `--lines`/`-n`, not `--limit`; drop `-f` unless you intend to block.
- `-w` / `--workspace` accepts name or id and can go before the verb or after the job name.
- Do not `runs attach` from an agent session. To wait for a run, use `runs wait <id> --timeout 9m` (exit 3 means still running; call it again) or `runs watch <id|job> --timeout 9m`. Do not write a poll loop over `runs show`. For the assistant, poll `conversation messages`. Do not `runs kill` a live session to re-prompt.
- Report only what a call returned. Do not carry a value from one call into a claim about another: if a filtered read returned nothing, say it returned nothing, and do not borrow a count or a cursor from the unfiltered read next to it. If a read failed or came back `not_found`, say so — do not claim what you would have seen. Reporting a failure plainly is a good answer; an unsupported claim is not.
