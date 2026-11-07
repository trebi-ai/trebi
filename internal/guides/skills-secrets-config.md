---
topic: skills-secrets-config
title: Skills, secrets, harnesses, and config
description: Stored skills, secrets, config keys, and harness auth.
---

# Skills, secrets, harnesses, and config

## Skills

```bash
trebi skill list --json
trebi skill show <name> --json
trebi skill create <name> --file ./SKILL.md
trebi skill create <name> --source-git https://example.com/deploy.git
trebi skill create <name> --source-folder ./bundles/deploy
trebi skill refresh <name>
trebi skill delete <name> --force
```

A skill has one of four scopes. The folder gives the scope:

| Scope | Folder | Runs that get it |
|---|---|---|
| `global` | `~/.trebi/skills/<name>` | all runs |
| `workspace` | `<workspace>/skills/<name>` | the runs in the workspace |
| `agent` | `<workspace>/agents/<agent>/skills/<name>` | the runs of the agent and of its jobs |
| `job` | `<job folder>/skills/<name>` | the runs of the job |

A run gets the skills of its job, its agent, its workspace, and the global skills. When two skills have the same name, the narrower one wins: job, then agent, then workspace, then global. `--scope` and `--owner` pick the scope and the owner (a workspace, agent, or job name or ID). `--workspace` finds an agent or job name.

```bash
trebi skill create deploy --scope workspace --owner acme --file ./SKILL.md
trebi skill create deploy --scope agent --owner cmo --workspace acme --file ./SKILL.md
trebi skill list --scope agent --owner cmo --workspace acme --json
trebi skill list --effective-for cmo --workspace acme --json
trebi skill show deploy --scope agent --owner cmo --workspace acme
trebi skill move deploy --scope agent --owner cmo --workspace acme --to-scope workspace --to-owner acme
trebi skill approve deploy --scope job --owner agents/cmo/report --workspace acme
```

`--effective-for` lists the skills that the runs of a job or agent get. Each row has `used`, and `overwritten_by` when a narrower skill wins. When a name is in more than one scope and you do not give `--scope`, a read gets the global skill and a write fails with `ambiguous` and the refs. `move` gives `conflict` when the target has a skill of the same name; add `--replace` or `--rename <name>`.

Who can write: a person and the assistant write all scopes (the assistant only in its own workspace). An agent run writes its own agent skills and the skills of its jobs. A job run writes only its own job skills. Any other write by a run fails with `forbidden`. To change a wider skill in a run, create a copy with the same name in your own scope; it overwrites the wider skill for your runs. In a run, `trebi skill create <name>` with no `--scope` creates the skill in the scope of the run. Add `--note` to tell the person why, for example to ask them to share it wider. The skill shows in the inbox. The agent kit key `skill_policy` controls this: `create: false` refuses all skill writes of the agent and its jobs, and `review: true` keeps a new skill pending until a person runs `trebi skill approve`.

A source bundle must hold a `SKILL.md`. The skill keeps the source, so `trebi skill refresh <name>` pulls it again; the reply reports `updated`. A skill with no source refreshes to no change.

`trebi skill list` also shows **inherited** skills discovered from the user harness skill dirs the bridge injects (`~/.claude/skills`, `~/.codex/skills`, `~/.grok/skills`, `~/.config/opencode/skills`, `~/.cursor/skills-cursor`, `~/.pi/agent/skills`) and the shared Agent Skills dir (`~/.agents/skills`). A `SKILL.md` that is a single `@path` line (Claude's pointer form) is followed to that file; the row's path is the directory that holds the real document. Those rows are read-only: edit, refresh, delete, and file writes are refused. Edit the origin folder instead. Inherited rows count as global. Managed rows are the ones in `~/.trebi/skills` and in the `skills` folders of workspaces, agents, and jobs. A folder named `skills` in those places holds skills only: Trebi does not read a job in it.

`--file` is **local only**; remote: pass `content`. Create the skill first, then put scripts with `trebi skill file`. `SKILL.md` writes 409 on the file command — use `trebi skill create` / the update path. File writes are denied for non-local provenance (git/registry/connector). `skill delete` is destructive — ask first.

```bash
trebi skill file list <skill> --json --limit 50
trebi skill file get <skill> helper.py
trebi skill file put <skill> helper.py --from ./helper.py
trebi skill file remove <skill> helper.py
trebi skill file put deploy run.sh --from ./run.sh --scope agent --owner cmo --workspace acme
```

`--from` is **local only**; remote: pass `content`. The file commands take the same `--scope` and `--owner`. Same If-Match rules as `trebi guide files`.

## Secrets

```bash
trebi secret list --json
trebi secret set --from-file ./api-key.txt eval_api_key
trebi secret delete eval_api_key
```

`--from-file` is **local only**; remote: pass `value`. Values are never printed. `secret list` is names only. `secret delete` is destructive — ask first.

On the MCP surface `secret set` takes `value_file` for the same reason: pass the path and `trebi mcp serve` reads it, so the value never enters your transcript. Do not read the file yourself first. Over plain HTTP, where no local bridge runs, `value_file` fails and you pass `value` instead. That is the supported fallback, not a leak: the `set` call takes a value, and no read on that surface is quiet. Read the key once, pass it to `secret set`, and do not show it again — a second `cat`, the value in your final message, or the value copied into a job prompt or a file all expose it where nothing needed it.

Reference a stored secret from a job by putting the **secret name** as the env value. The daemon resolves it at launch. Never put the secret value in the prompt, JOB.md, or a command line.

```markdown
---
harness: codex
env:
  EVAL_API_KEY: eval_api_key
---

Print the names of environment variables that start with EVAL_, never their values.
```

## Harnesses

A harness is the agent tool that runs the session (claude, codex, …).

```bash
trebi harness list --json
trebi harness enable <name> [--mode shared|separate]
trebi harness disable <name>
trebi harness set <name> --model <model> --effort <level>
trebi harness providers opencode
trebi harness ensure <name>
trebi harness auth status <name> --json
trebi harness auth status <name> --live --json
```

Each harness is off until you turn it on. A job on a harness that is off does not run, and `job validate` says so. You cannot turn off the default harness for new jobs.

```bash
trebi harness auth login <name>
trebi harness auth login <name> --token-stdin
trebi harness auth login opencode --token-stdin --provider anthropic
trebi harness auth login <name> --attach
trebi harness auth logout <name> --yes
trebi harness auth mode <name> shared
trebi harness auth mode <name> separate
trebi harness auth cancel <name>
```

`harness auth login --attach` is **local only** (TUI over WebSocket); remote: `login_start` / `login_token` / `login_code`. `--token-stdin` is **local only**; remote: pass `token`. Cursor has no device flow — use `--token-stdin` or log in on the daemon machine.

Login runs on the daemon. The CLI prints the verification URL + user code; complete OAuth in any browser and paste the code back. On macOS, claude credentials live in the Keychain; `trebi harness auth login claude` runs `setup-token` in both shared and separate mode.

### Variants

A variant is a harness that you make. It runs the program of its base (only `claude` now) with its own name, folder, login, and env. Use a variant to run Claude Code against another service, or with another account. A job picks a variant with `harness: <name>`, like any harness.

```bash
trebi harness add claude-zai --base claude --label "Claude Z.ai" --model glm-5
trebi harness env set claude-zai ANTHROPIC_BASE_URL https://api.z.ai/api/anthropic
trebi harness env set claude-zai ANTHROPIC_AUTH_TOKEN --secret < token.txt
trebi harness env rm claude-zai ANTHROPIC_MODEL
trebi harness set claude-zai --label "Claude Z.ai"
trebi harness rm claude-zai --move-to claude
```

- `env set --secret` reads the value from stdin when you give no value. The secret goes to the secret store, and `harness list --json` never shows it.
- Trebi sets `CLAUDE_CONFIG_DIR` and `TREBI_*`, so `env set` refuses them.
- `harness rm` refuses a variant that jobs, the default, or the assistant use. Give `--move-to <harness>` to move them first.
- The assistant never takes a secret value in chat. Ask the user to type it on the harness page.
- `--mode separate` gives the variant a login of its own. Then log it in with `harness auth login <name>`.

## Config

```bash
trebi config show --json
trebi config show --yaml
trebi config show --tree
trebi config show --key tools.grok --json
trebi config show --key defaults.command --json
trebi config set daemon.log_level debug
trebi config set api.host 0.0.0.0
trebi config set api.port 7433
trebi config set api.tls.insecure true
trebi config set api.tls.cert /path/cert.pem
trebi config set api.tls.key /path/key.pem
trebi config set assistant.native.model claude-sonnet-4-5
trebi config set assistant.native.api_format anthropic
trebi config set assistant.native.api_key_env ANTHROPIC_API_KEY
trebi config unset assistant.api_key
```

`show` lists every known key, not only the ones written to config.yaml. When the daemon is up, `show` reports the server config. Bare `trebi config` prints the help menu.

```bash
trebi config path
trebi config validate
trebi config bridge status --json
trebi config bridge set claude credentials inherit
```

`config path`, `config validate`, and `config bridge` are **local only**; remote: `config.show` / `config.set`. `config validate` works with the daemon down.

Schema, YAML view, and draft checks go through the daemon:

```bash
trebi config schema --json
trebi config schema --key notify.route --json
trebi config yaml
trebi config validate --set notify.usage_warn_at=90 --unset defaults.workspace --json
trebi config validate --file ./draft.yaml
```

`schema` gives each key its `type` (`string|int|bool|duration|list|map|json`), `enum`, `default`, `secret`, and `restart`. A `restart` key takes effect after the daemon restarts. `yaml` masks secret values as `***`; a `set` with `***` keeps the stored value. `validate` with a draft applies the change to a copy and writes nothing. It answers `{ok, errors: [{key, message}]}`. A JSON value such as `notify.route` goes as JSON text: `--set 'notify.route=[{"via":"email","to":"admins"}]'`.
