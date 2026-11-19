---
topic: agents
title: Folder-scoped agents
description: Create an agent, give it jobs, wake it, and manage its memory.
---

# Folder-scoped agents

An agent owns the folder `agents/<name>/` inside a workspace. The document `AGENT.md` holds YAML front matter and the instruction text. `title:` is the human-facing display name; the folder name remains the one identifier. A wake starts one session on the agent. The daemon composes the prompt from the agent instructions, the owned jobs, and the agent memory. Each job belongs to one agent.

The assistant is the workspace chat agent, not an agent (`trebi guide assistant`). An agent is narrower: it sees its folder only. Keep one agent per duty, for example `cmo` for growth reports. The name uses lowercase letters, digits, and hyphens. It starts with a letter or a digit. The agent name is the folder name; runs and schedules key on it. The names `assistant` and `nobody` are reserved. The hidden `trebi agent-hook` command is a harness lifecycle hook. It shares only the word.

## Create an agent

```bash
trebi agent create cmo --workspace reports --prompt "Run growth." --cron "0 3 * * *"
trebi agent create cmo --workspace reports --file ./AGENT.md
trebi agent list --workspace reports --json
trebi agent show cmo --workspace reports --json
```

`create` is an upsert: PUT on a new name creates the agent. Give `--prompt` and a schedule flag (`--cron` or `--every`), and the daemon composes the front matter. An agent gets the messages of a connection through a route: see `trebi guide routing`. Give `--file` for full control (**local only**; remote: pass `content`). With neither, the daemon seeds a default document and a memory ledger. Set `harness` only to a harness that `trebi harness list` reports with `enabled: true` and `auth_ok: true`.

Inbound agent mail is closed unless `AGENT.md` explicitly accepts it:

```yaml
email:
  accept:
    - admins           # config.yaml email.admins
    - allowlist        # trebi.yaml email.allowlist
    - alice@acme.com   # exact address
    - "@acme.com"      # exact domain
```

The assistant has the same `email.accept` key in the `assistant:` block of the workspace `trebi.yaml`. If it is absent, the assistant accepts `admins` only. Add `allowlist` to let allowlisted senders write to it.

The same list is also the send rule of the agent: an agent sends to admins, the workspace allowlist, and the addresses its `email.accept` accepts. An owned job sends to those and to the `allowlist` of its own routes on the email connection. An empty route `allowlist` adds no address.

Mail from Trebi has the house style. `email.style` changes it:

```yaml
email:
  style:
    accent: "#0f766e"                  # links, report header, and accent bar
    logo: https://acme.com/logo.png    # https image at the top of a report
    footer: Acme Ops · weekly          # one line below each mail
```

Put `email.style` in the `assistant:` block of `trebi.yaml` to set the style of the workspace. An agent `email.style` changes only the fields it sets. The purpose of the mail selects the layout: a report or a send gets the report layout, a reply gets the reply layout, and an alert or a test gets the minimal layout. `trebi guide email-format` shows the layouts and the report blocks.

## Rename identity

```bash
trebi agent rename cmo growth --workspace reports
```

Rename moves the whole folder and rewrites runs, queued events, schedules, and email history in one transaction. Earlier runs resume in the new folder. It refuses an existing target or a live run. The agent email address changes with the name. Add `--dry-run` to see the new address and the agents whose `team.can_ask` lists the old name; the rename does not change those agents.

## Templates, kits, budget, and reports

List onboarding templates or create a disabled starter portfolio:

```bash
trebi agent templates
trebi agent create research --workspace reports --template research \
  --title "Research" --accept admins --input competitors="acme,beta"
```

`AGENT.md kit:` carries reusable `env`, `skills`, and `connectors` values. Owned jobs inherit it; their own `env` wins, and their own `connectors` adds to the kit list. `skills` stays a prompt hint and a doctor check; `skill_policy` (`create: false` refuses the skill writes of the agent and its jobs, `review: true` keeps a new skill pending until a person approves it) controls the skills that the agent makes; `connectors` names managed connectors whose actions are injected into each run (see `trebi guide connectors-and-rules`). The built-in name `email` gives each run the `trebi email send` command and the sender: an agent sends as itself, and an owned job sends as its owner agent. A new agent has `email` in its kit. Do not repeat the send command in a job; write only the recipients, the subject, and the body the job wants.

An agent has its own skills in `agents/<agent>/skills/<name>/`, and a job in `<job folder>/skills/<name>/`. The runs of the agent and of its jobs get the agent skills. A skill there overwrites a workspace or global skill of the same name. See `trebi guide skills-secrets-config`.

Optional budget and report keys are off by default:

```yaml
budget:
  monthly_usd: 50
  on_exceed: pause # or alert
report:
  every: 168h
  to: [admins]
```

`pause` refuses automated work after the limit; a human manual launch always runs. `report` creates an internal schedule, wakes the agent with portfolio context, and mails the final brief from the agent subaddress to the resolved recipients.

## Give the agent jobs

```bash
trebi job create weekly-digest --workspace reports --prompt "Write the digest." --agent cmo
```

A job created with `--agent` lands in `agents/<name>/`. The full job name is the agent name, a slash, and the job name, for example `cmo/weekly-digest`. The job belongs to the agent: a wake of the agent covers it, and `agent delete` moves it to another agent (`--move-jobs-to`) or removes it (`--purge`). Each job belongs to an agent. A `JOB.md` outside `agents/<name>/` is a parse error and does not run. Read and edit it with the `job` commands under that full name.

`working_dir` sets the folder where runs start. Put it in `AGENT.md` to set it for the agent and for each job it owns:

```yaml
working_dir: ~/clients/acme   # ~ expands; a relative path starts at the AGENT.md folder
```

An owned job uses its own `working_dir` first, then the `working_dir` of its agent, then its `JOB.md` folder. An agent with no `working_dir` runs in the workspace root. `trebi job show <name> --json` gives the folder in `working_dir` and its source in `working_dir_source` (`job`, `agent`, or `default`). A resumed run keeps the folder of the run it resumes. `trebi doctor` reports a `working_dir` that is not a folder (`working_dir_missing`).

## Wake

```bash
trebi agent wake cmo --workspace reports --test --json
trebi agent wake cmo --workspace reports
trebi agent wake cmo --workspace reports --extra-prompt "focus on the weekly numbers"
```

Use `--test` before the first real launch **only if this session created or edited that AGENT.md**; it evaluates the definition and writes nothing. `--attach` is **local only**; remote: poll `runs show` / `runs transcript`.

An accepted inbound thread is a real conversation bound to the agent operator, not a one-shot wake. `conversations list --agent cmo` shows it; the daemon replies on the mail thread from the agent subaddress after the exchange ends. The runs answer is `runs list`:

```bash
trebi runs list --workspace reports --kind agent --job cmo --json
trebi runs list --workspace reports --job cmo/weekly-digest --json
```

## Ask and hand off

```bash
trebi agent list --workspace reports --for "budget"
trebi agent ask cfo "What is the Q3 budget?" --workspace reports --context "The board meets Friday." --wait 60s
trebi agent handoff legal "Review docs/nda.md." --workspace reports
trebi chain list --workspace reports --live
trebi chain show <trace-id or run-id>
trebi chain hop <hop-id>
```

An ask starts or continues a run of the target. The last message of that turn is the reply. A hand-off gives the work away; the caller gets no reply. All the work that one start causes is one chain. The workspace settings `agents.chains` set its limits: depth (`max_hops`), turns (`max_turns`), and cost (`max_usd`). Inside a run, end your turn after `agent ask`. The reply starts your next turn. `--wait` works only outside a run. An ask fails with `chain_cycle` when the target already waits for the caller: reply instead.

## Disable and delete

```bash
trebi agent patch cmo --workspace reports --disabled
trebi agent delete cmo --workspace reports --force
trebi agent delete cmo --workspace reports --move-jobs-to growth --force
trebi agent delete cmo --workspace reports --purge --force
```

Ask the user first. Delete removes the whole agent folder. An agent with jobs needs a choice: `--move-jobs-to <agent>` gives the jobs to another agent first, and `--purge` deletes them with the agent. Without a choice, an agent with jobs fails and names the count. A moved job keeps its ID, runs, schedules, state, memory, and folder. `agent memory clear` is destructive too — ask first.

## Archive an agent

An archived agent and all its jobs keep their files, runs, memory, and state, but nothing starts. Ask the user first. Only a person or the assistant archives an agent, never an agent run.

```bash
trebi agent archive cmo --workspace reports
trebi agent unarchive cmo --workspace reports
trebi agent list --workspace reports --archived
trebi agent list --workspace reports --include-archived
```

Archive writes `archived_at` in `AGENT.md`. The jobs of the agent show as archived but keep their own files. A start, a wake, an ask, a hand-off, an escalation, and a rule fire for the agent fail with the code `archived`. A mail to the agent goes on as if no agent matched. Unarchive restores the agent and the jobs that were not archived alone. `agent patch` cannot set `archived_at`.

## Agent memory

Each agent keeps a structured `MEMORY.md` at `agents/<name>/MEMORY.md`. It survives between runs and feeds every wake. It holds three sections in a fixed order: Context, Lessons, Log. Keep records in `trebi state`, not in memory. The daemon validates it at wake completion, like job memory.

```bash
trebi agent memory show cmo --workspace reports --json
trebi agent memory validate cmo --workspace reports
trebi agent memory set-context --body context.md cmo --workspace reports
trebi agent memory lesson add cmo --workspace reports --text "Learned the API"
trebi agent memory log add cmo --workspace reports --text "woke, shipped the digest"
trebi agent memory log delete cmo --workspace reports <entry-id>
trebi agent memory set cmo --workspace reports --file ./MEMORY.md
trebi agent memory clear cmo --workspace reports --force
```

`--body` and `--file` are **local only**; remote: pass `body` / `content`. `memory clear` is destructive — ask first. Offline repair: `trebi memory validate <path> --fix`. Job memory (per job, not the agent): `trebi guide jobs-and-scheduling`.

## Agent state

An agent keeps records in `trebi state`, the same as a job. Declare collections with `state:` in `AGENT.md`. Each wake gets a `## State` section. An owned job has its own state, separate from the agent state.

```bash
trebi state --owner cmo --workspace reports
trebi state query leads --owner cmo --workspace reports --since 7d
```

In a wake, the owner is `self` by default. A run cannot read or write the state of another job or agent (`state_scope`). Commands and frontmatter: `trebi guide jobs-and-scheduling`.
