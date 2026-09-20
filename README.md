# trebi

Trebi is an LLM task runner with an HTTP `/v1` API, SSE event bus, SQLite store, and `trebi` CLI.

This tree implements the contract in [docs/trebi/backend-0808.md](../docs/trebi/backend-0808.md).

## Layout

```
trebi/
  cmd/trebi/           # single binary (CLI + daemon): app.Main(ext.Set{})
  app/                 # public entry point
  ext/                 # public extension contract (wrapping editions)
  internal/
    model/             # enums, wire types, event registry
    store/             # SQLite schema + bus outbox
    home/              # TREBI_HOME paths
    job/               # canonical frontmatter parse/write
    workspaces/        # registry + job_index
    runs/              # runs, timeline, agent state
    dispatch/          # Goja match/extract, policy, queue
    pricing/           # token → cost_usd
    api/               # /v1 HTTP + SSE + /mcp
    cli/               # urfave/cli commands
    guides/            # embedded workflow cheat sheets
    ui/                # embedded SPA (build tag ui; dist is git-ignored)
  scripts/
    eval.py                    # uv: run/oracle/review + report/ls/why/compare
    build.sh                   # dev build with a stamped version (embeds the UI; --no-ui to skip)
    semver-ge.sh               # release compatibility rule
```

## Build & test

```bash
cd trebi
go test ./...
go build -o ../build/trebi ./cmd/trebi
./../build/trebi --help
```

`scripts/build.sh` wraps the build, stamps `internal/api.Version` from
`git describe --tags --always --dirty`, and restarts the LaunchAgent when one
is installed. It embeds the UI by default: it builds the sibling `../trebi-ui`
working tree (override with `TREBI_UI_SRC`), copies its `apps/app/dist` into
git-ignored `internal/ui/dist`, and builds with `-tags ui`. The UI version is
stamped from that checkout's `git describe` too (e.g.
`0.1.0-2-g5fd0d06-dirty`); `TREBI_UI_VERSION` overrides it, and the release
job keeps its npm-stamped version. Pass `--no-ui` for a plain binary. Only the
release workflow downloads a `trebi-ui` release tarball; local builds never do.

## Releases

This repository does not release. `trebi-pro` owns the tags and the releases,
and it creates them here, so the download URLs stay on the public repository.
One binary ships, `trebi_*`, with `SHA256SUMS`. Pro features live in that same
binary and a license unlocks them at run time. The full scheme is
`../docs/plans/2026-09-18-release-versioning.md` plus
`../docs/plans/2026-09-19-open-core-build.md`.

`.github/workflows/check.yml` is the gate (test, lint, and the fixture-backed
`-tags ui` build) and runs on every push and PR, `main` included. A green
`main` then runs `.github/workflows/dispatch.yml`, which tells `trebi-pro` to
tag this commit and cut a release.

- A binary reports `version` and its `edition`. It also reports `ui_version` on
  `/v1/system/health` and `/v1/system/overview` when a SPA is embedded; the
  field is omitted when none is. `edition_version` is omitted on a free build.
- The `-tags ui` build stays for local use and for the gate's fixture build.
  Only `trebi-pro` embeds a `trebi-ui` release into a shipped binary.
- `scripts/semver-ge.sh` is the compatibility rule (strip `v`, compare
  `major.minor.patch`, pass on a dev build) and `scripts/semver-ge.test.sh` is
  its table test.


## Daemon (temp home)

```bash
export TREBI_HOME=/tmp/trebi-dev
./build/trebi service migrate-db
./build/trebi service daemon          # binds 127.0.0.1:7433 by default
# other terminal
./build/trebi service status
./build/trebi runs list
```

## MCP

The daemon also serves Streamable HTTP MCP at `/mcp`, with the same bearer token as `/v1`. The daemon must be running.

`trebi mcp` writes the client config for you:

```bash
trebi mcp config --client claude             # the config block
trebi mcp config --client claude --command   # the add command
trebi mcp install --client claude            # runs the add command
trebi mcp serve                              # stdio bridge for a client without HTTP
```

Clients: `claude`, `codex`, `opencode`, `cursor`, `vscode`, and `json` for the raw url and token. The sections below show the same blocks, filled in by hand:

```bash
trebi service status --json          # confirms the API is up
URL=$(jq -r .url ~/.trebi/api.json)/mcp
TOKEN=$(trebi service token show)    # same value as ~/.trebi/api.json "token"
```

Auth is `Authorization: Bearer <token>`. Trebi does not speak OAuth. Prefer env vars or `{env:…}` substitution over putting the token in a committed file.

Tools take `{"action": "<name>", "input": {…}}`. Call `docs` on a tool for its fields, and `trebi guide mcp-basics` (or MCP `system.guide` with topic `mcp-basics`) for the CLI→MCP map. There is no sticky workspace over MCP: pass `workspace` in `input` on every workspace-scoped call.

### OpenCode

```bash
opencode2 mcp add trebi --global \
  --url "$URL" \
  --header "Authorization=Bearer $TOKEN"
```

Drop `--global` for project-only. OpenCode defaults remote servers to OAuth; Trebi is header-only, so set `"oauth": false` on the server in `opencode.json(c)`:

```jsonc
{
  "mcp": {
    "servers": {
      "trebi": {
        "type": "remote",
        "url": "http://127.0.0.1:<port>/mcp",
        "oauth": false,
        "headers": {
          "Authorization": "Bearer {env:TREBI_TOKEN}"
        }
      }
    }
  }
}
```

Then `opencode2 mcp list`. Do not run `opencode2 mcp auth trebi`.

### Claude Code

```bash
claude mcp add --transport http trebi "$URL" \
  --header "Authorization: Bearer $TOKEN" \
  --scope user
```

`--scope user` is all projects; omit it (or `--scope local`) for this project only; `--scope project` writes `.mcp.json`. Equivalent JSON (`~/.claude.json`, `.mcp.json`, or `~/.claude/mcp.json`):

```json
{
  "mcpServers": {
    "trebi": {
      "type": "http",
      "url": "http://127.0.0.1:<port>/mcp",
      "headers": {
        "Authorization": "Bearer ${TREBI_TOKEN}"
      }
    }
  }
}
```

`type` is required. Then `claude mcp list`.

### Codex

```bash
export TREBI_TOKEN="$TOKEN"
codex mcp add trebi --url "$URL" --bearer-token-env-var TREBI_TOKEN
```

Or in `~/.codex/config.toml` (shared with the ChatGPT desktop app and IDE extension):

```toml
[mcp_servers.trebi]
url = "http://127.0.0.1:<port>/mcp"
bearer_token_env_var = "TREBI_TOKEN"
```

Do not run `codex mcp login trebi`.

### Cursor

`~/.cursor/mcp.json` (all projects) or `.cursor/mcp.json` (this project):

```json
{
  "mcpServers": {
    "trebi": {
      "url": "http://127.0.0.1:<port>/mcp",
      "headers": {
        "Authorization": "Bearer ${env:TREBI_TOKEN}"
      }
    }
  }
}
```

### VS Code

`.vscode/mcp.json` (workspace) or **MCP: Open User Configuration**:

```json
{
  "servers": {
    "trebi": {
      "type": "http",
      "url": "http://127.0.0.1:<port>/mcp",
      "headers": {
        "Authorization": "Bearer ${env:TREBI_TOKEN}"
      }
    }
  }
}
```

Or **MCP: Add Server** → HTTP, URL `$URL`, header `Authorization: Bearer $TOKEN`.

### Claude Desktop

`~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows). If the app accepts remote HTTP:

```json
{
  "mcpServers": {
    "trebi": {
      "type": "http",
      "url": "http://127.0.0.1:<port>/mcp",
      "headers": {
        "Authorization": "Bearer TOKEN"
      }
    }
  }
}
```

If it only launches stdio servers, bridge with `mcp-remote`:

```json
{
  "mcpServers": {
    "trebi": {
      "command": "npx",
      "args": [
        "-y",
        "mcp-remote",
        "http://127.0.0.1:<port>/mcp",
        "--header",
        "Authorization: Bearer TOKEN"
      ]
    }
  }
}
```

Quit and relaunch Claude Desktop after editing.

## Migration (dry-run only until you cut over)

```bash
uv run scripts/migrate-jobi-to-trebi.py status
uv run scripts/migrate-jobi-to-trebi.py migrate --shadow
uv run scripts/migrate-jobi-to-trebi.py rewrite-jobs --diff
uv run scripts/migrate-jobi-to-trebi.py rewrite-jobs --job receipts --target-workspace ellis
uv run scripts/migrate-jobi-to-trebi.py migrate --agent scout --target-workspace ellis
uv run scripts/migrate-jobi-to-trebi.py convert-memory path/to/MEMORY.md
uv run scripts/migrate-jobi-to-trebi.py verify
```

`--job` limits rewrite and DB copy to one job; `--agent` limits both to one agent and its jobs (requires `--target-workspace`) and lands them at `agents/<name>/<job>/`. `--target-workspace` is the trebi destination (id, name, or path). Default is dry-run; `--apply` writes. Incremental `--job`/`--agent --apply` copies into the target and leaves `~/.jobi` files untouched. Full `rewrite-jobs --apply` without `--job` still rewrites in place (cutover). `rewrite-jobs` converts jobi `MEMORY.md` (extra `##` headings) to trebi v1; `convert-memory` does that without a full rewrite. `--apply` refuses when a trebid is running.

The plan's preflight reports agent-layout problems (AGENT.md outside `agents/<name>/`, SOUL.md or JOB.md beside AGENT.md) because trebi's discovery refuses those files. `make-fixture DIR` writes a scrubbed jobi tree with agent jobs for rehearsing the flow.

## Conventions

- Names: run, workspace, connector, harness (not task/source/tool/integration).
- Canonical frontmatter only (`harness` — plain name or `{name, model, effort}` — plus `triggers`, `envset`, `env`, `session.idle_timeout`).
- Bus topics and log event names share one registry (`internal/model/registry.go`).
- Entity write + `event_log` append commit in one transaction (outbox).
