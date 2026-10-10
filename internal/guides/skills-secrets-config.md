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
trebi skill delete <name>
```

A source bundle must hold a `SKILL.md`. The skill keeps the source, so `trebi skill refresh <name>` pulls it again; the reply reports `updated`. A skill with no source refreshes to no change.

`trebi skill list` also shows **inherited** skills discovered from the user harness skill dirs the bridge injects (`~/.claude/skills`, `~/.codex/skills`, `~/.grok/skills`, `~/.config/opencode/skills`, `~/.cursor/skills-cursor`, `~/.pi/agent/skills`) and the shared Agent Skills dir (`~/.agents/skills`). A `SKILL.md` that is a single `@path` line (Claude's pointer form) is followed to that file; the row's path is the directory that holds the real document. Those rows are read-only: edit, refresh, delete, and file writes are refused. Edit the origin folder instead. Managed rows are the ones in `~/.trebi/skills`.

`--file` is **local only**; remote: pass `content`. Create the skill first, then put scripts with `trebi skill file`. `SKILL.md` writes 409 on the file command — use `trebi skill create` / the update path. File writes are denied for non-local provenance (git/registry/connector). `skill delete` is destructive — ask first.

```bash
trebi skill file list <skill> --json --limit 50
trebi skill file get <skill> helper.py
trebi skill file put <skill> helper.py --from ./helper.py
trebi skill file remove <skill> helper.py
```

`--from` is **local only**; remote: pass `content`. Same If-Match rules as `trebi guide files`.

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
