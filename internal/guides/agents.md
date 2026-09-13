---
topic: agents
title: Folder-scoped agents
description: Create an agent, give it jobs, wake it, and manage its memory.
---

# Folder-scoped agents

An agent owns the folder `agents/<name>/` inside a workspace. The document `AGENT.md` holds YAML front matter and the instruction text. A wake starts one session on the agent. The daemon composes the prompt from the agent instructions, the owned jobs, and the agent memory. Workspace jobs stay out of it.

The assistant is the workspace singleton (`trebi guide assistant`). An agent is narrower: it sees its folder only. Keep one agent per duty, for example `cmo` for growth reports. The name uses lowercase letters, digits, and hyphens. It starts with a letter or a digit. The index name is `agents/<name>`; runs and schedules key on it. The hidden `trebi agent-hook` command is a harness lifecycle hook. It shares only the word.

## Create an agent

```bash
trebi agent create cmo --workspace reports --prompt "Run growth." --cron "0 3 * * *"
trebi agent create cmo --workspace reports --on mail --match '.subject'
trebi agent create cmo --workspace reports --file ./AGENT.md
trebi agent list --workspace reports --json
trebi agent show cmo --workspace reports --json
```

`create` is an upsert: PUT on a new name creates the agent. Give `--prompt` and a trigger flag, and the daemon composes the front matter. Give `--file` for full control (**local only**; remote: pass `content`). With neither, the daemon seeds a default document and a memory ledger. Set `harness` only to a harness that `trebi harness list` reports with `auth_ok: true`.

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

Use `--test` before the first real launch **only if this session created or edited that AGENT.md**; it evaluates the definition and writes nothing. `--attach` is **local only**; remote: poll `runs show` / `runs transcript`. Agents keep no chat loop: no `agent say`, no history. The runs answer is `runs list`:

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

Each agent keeps a `MEMORY.md` ledger at `agents/<name>/MEMORY.md`. It survives between runs and feeds every wake.

```bash
trebi agent memory show cmo --workspace reports --json
trebi agent memory add cmo --workspace reports --text "Learned the API"
trebi agent memory set cmo --workspace reports --file ./MEMORY.md
trebi agent memory delete cmo --workspace reports <entry-id>
trebi agent memory clear cmo --workspace reports --force
```

`--file` on `memory set` is **local only**; remote: pass `content`. Job memory (per job, not the agent): `trebi guide jobs-and-scheduling`.
