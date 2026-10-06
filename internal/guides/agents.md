---
topic: agents
title: Folder-scoped agents
description: Create an agent, give it jobs, wake it, and manage its memory.
---

# Folder-scoped agents

An agent owns the folder `agents/<name>/` inside a workspace. The document `AGENT.md` holds YAML front matter and the instruction text. `title:` is the human-facing display name; the folder name remains the one identifier. A wake starts one session on the agent. The daemon composes the prompt from the agent instructions, the owned jobs, and the agent memory. Workspace jobs stay out of the index rollup.

The assistant is the workspace chat agent, not an agent (`trebi guide assistant`). An agent is narrower: it sees its folder only. Keep one agent per duty, for example `cmo` for growth reports. The name uses lowercase letters, digits, and hyphens. It starts with a letter or a digit. The index name is `agents/<name>`; runs and schedules key on it. The hidden `trebi agent-hook` command is a harness lifecycle hook. It shares only the word.

## Create an agent

```bash
trebi agent create cmo --workspace reports --prompt "Run growth." --cron "0 3 * * *"
trebi agent create cmo --workspace reports --on mail --match '.subject'
trebi agent create cmo --workspace reports --file ./AGENT.md
trebi agent list --workspace reports --json
trebi agent show cmo --workspace reports --json
```

`create` is an upsert: PUT on a new name creates the agent. Give `--prompt` and a trigger flag, and the daemon composes the front matter. Give `--file` for full control (**local only**; remote: pass `content`). With neither, the daemon seeds a default document and a memory ledger. Set `harness` only to a harness that `trebi harness list` reports with `auth_ok: true`.

Inbound agent mail is closed unless `AGENT.md` explicitly accepts it:

```yaml
email:
  accept:
    - admins           # config.yaml email.admins
    - allowlist        # trebi.yaml email.allowlist
    - alice@acme.com   # exact address
    - "@acme.com"      # exact domain
```

The assistant has the same `email.accept` key in the `assistant:` block of the workspace `trebi.yaml`. If it is absent, the assistant keeps the `admins` plus `allowlist` rule.

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

Rename moves the whole folder and rewrites runs, queued events, schedules, and email history in one transaction. It refuses an existing target or a live run.

## Templates, kits, budget, and reports

List onboarding templates or create a disabled starter portfolio:

```bash
trebi agent templates
trebi agent create research --workspace reports --template research \
  --title "Research" --accept admins --input competitors="acme,beta"
```

`AGENT.md kit:` carries reusable `envset` (a name or a list of names), `env`, `skills`, and `connectors` values. Owned jobs inherit it; their own `envset`/`env` wins, and their own `connectors` adds to the kit list. `skills` stays a prompt hint and a doctor check; `connectors` names managed connectors whose actions are injected into each run (see `trebi guide connectors-and-rules`). The built-in name `email` gives each run the `trebi email send` command and the sender: an agent sends as itself, and an owned job sends as its owner agent. A new agent has `email` in its kit. Do not repeat the send command in a job; write only the recipients, the subject, and the body the job wants.

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

A job created with `--agent` lands in `agents/<name>/`. The full job name is the folder path, for example `agents/cmo/weekly-digest`. The job belongs to the agent: a wake of the agent covers it, and `agent delete --purge` removes it. Read and edit it with the `job` commands under that full name.

## Wake

```bash
trebi agent wake cmo --workspace reports --test --json
trebi agent wake cmo --workspace reports
trebi agent wake cmo --workspace reports --extra-prompt "focus on the weekly numbers"
```

Use `--test` before the first real launch **only if this session created or edited that AGENT.md**; it evaluates the definition and writes nothing. `--attach` is **local only**; remote: poll `runs show` / `runs transcript`.

An accepted inbound thread is a real conversation bound to the agent operator, not a one-shot wake. `conversations list --agent cmo` shows it; the daemon replies on the mail thread from the agent subaddress after the exchange ends. The runs answer is `runs list`:

```bash
trebi runs list --workspace reports --kind agent --job agents/cmo --json
trebi runs list --workspace reports --job agents/cmo/weekly-digest --json
```

## Disable and delete

```bash
trebi agent patch cmo --workspace reports --disabled
trebi agent delete cmo --workspace reports
trebi agent delete cmo --workspace reports --purge
```

Ask the user first. Plain delete removes `AGENT.md` only; the owned jobs stay and keep their owner stamp. `--purge` removes the whole agent folder, owned jobs included. `agent memory clear` is destructive too — ask first.

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
