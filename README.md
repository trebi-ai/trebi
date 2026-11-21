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
```

## Build & test

```bash
cd trebi
go test ./...
go build -o ../build/trebi ./cmd/trebi
./../build/trebi --help
```

`go build` makes a plain binary with no embedded UI. `-tags ui` embeds the SPA from git-ignored `internal/ui/dist`; CI builds it with the fixture under `internal/ui/testdata/dist`. The release binary is built elsewhere and is not built from this repository.

## Releases

This repository does not release. `trebi-pro` owns the tags and the releases,
and it creates them here, so the download URLs stay on the public repository.
One binary ships, `trebi_*`, with `SHA256SUMS`. Pro features live in that same
binary. A Trebi Cloud account with the Pro plan unlocks them at run time; the
user links it in the UI. The full scheme is
`../docs/plans/2026-09-18-release-versioning.md` plus
`../docs/plans/2026-09-19-open-core-build.md`.

`.github/workflows/check.yml` is the gate: test, lint, and the fixture-backed
`-tags ui` build. It runs on PRs and on branch pushes. On `main` it runs
inside `.github/workflows/dispatch.yml`, which then tells `trebi-pro` to
release that commit. `trebi-pro` builds that exact commit and tags it here
after its builds pass.

- A binary reports `version` and its `edition`. It also reports `ui_version` on
  `/v1/system/health` and `/v1/system/overview` when a SPA is embedded; the
  field is omitted when none is. `edition_version` is omitted on a free build.
- The `-tags ui` build stays for local use and for the gate's fixture build.
  Only `trebi-pro` embeds a `trebi-ui` release into a shipped binary.


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

The daemon also serves Streamable HTTP MCP at `/mcp`. The daemon must be running.

`trebi mcp` writes the client config for you:

```bash
trebi mcp config --client claude             # the config block
trebi mcp config --client claude --command   # the add command
trebi mcp install --client claude            # runs the add command
trebi mcp serve                              # stdio bridge for a client without HTTP
trebi mcp config --remote                    # the Trebi Cloud URL for Claude and ChatGPT (Pro Cloud)
```

Clients: `claude`, `codex`, `opencode`, `opencode2`, `cursor`, `vscode`, and `json` for the raw url. The sections below show the same blocks, filled in by hand.

```bash
trebi service status --json          # confirms the API is up
URL=$(jq -r .url ~/.trebi/api.json)/mcp
```

**There is no auth.** The daemon serves `/v1`, `/mcp`, and the SPA without a credential, so every client config is a url and nothing else. Bind the daemon to a network you trust — loopback, or an overlay such as Tailscale. Non-loopback listen still requires TLS or the explicit insecure flag (`api.tls.*`, B28).

Tools take `{"action": "<name>", "input": {…}}`. Call `docs` on a tool for its fields, and `trebi guide mcp-basics` (or MCP `system.guide` with topic `mcp-basics`) for the CLI→MCP map. There is no sticky workspace over MCP: pass `workspace` in `input` on every workspace-scoped call.

### OpenCode

```bash
trebi mcp config --client opencode  --command   # opencode mcp add trebi --url "$URL"
trebi mcp config --client opencode2 --command   # opencode2 mcp add trebi --url "$URL"
```

Neither binary has a `--global` flag: `mcp add` writes `./opencode.json` in the current directory, so add the printed block to `~/.config/opencode/opencode.json` for a user-wide server. `--client opencode` prints the 1.x shape (`mcp.trebi`); `--client opencode2` prints the 2.x shape (`mcp.servers.trebi`), which is what the beta writes itself. OpenCode defaults remote servers to OAuth; this daemon has none, so both set `"oauth": false`:

```jsonc
{
  "mcp": {
    "servers": {
      "trebi": {
        "type": "remote",
        "url": "http://127.0.0.1:<port>/mcp",
        "oauth": false
      }
    }
  }
}
```

Then `opencode2 mcp list`. Do not run `opencode2 mcp auth trebi`.

### Claude Code

```bash
claude mcp add --transport http trebi "$URL" --scope user
```

`--scope user` is all projects; omit it (or `--scope local`) for this project only; `--scope project` writes `.mcp.json`. Equivalent JSON (`~/.claude.json`, `.mcp.json`, or `~/.claude/mcp.json`):

```json
{
  "mcpServers": {
    "trebi": {
      "type": "http",
      "url": "http://127.0.0.1:<port>/mcp"
    }
  }
}
```

`type` is required. Then `claude mcp list`.

### Codex

```bash
codex mcp add trebi --url "$URL"
```

Or in `~/.codex/config.toml` (shared with the ChatGPT desktop app and IDE extension):

```toml
[mcp_servers.trebi]
url = "http://127.0.0.1:<port>/mcp"
```

Do not run `codex mcp login trebi`.

### Cursor

`~/.cursor/mcp.json` (all projects) or `.cursor/mcp.json` (this project):

```json
{
  "mcpServers": {
    "trebi": {
      "url": "http://127.0.0.1:<port>/mcp"
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
      "url": "http://127.0.0.1:<port>/mcp"
    }
  }
}
```

Or **MCP: Add Server** → HTTP, URL `$URL`.

### Claude Desktop

`~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows). If the app accepts remote HTTP:

```json
{
  "mcpServers": {
    "trebi": {
      "type": "http",
      "url": "http://127.0.0.1:<port>/mcp"
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
        "http://127.0.0.1:<port>/mcp"
      ]
    }
  }
}
```

Quit and relaunch Claude Desktop after editing.

## Conventions

- Names: run, workspace, connector, harness (not task/source/tool/integration).
- Canonical frontmatter only (`harness` — plain name or `{name, model, effort}` — plus `triggers`, `env`, `session.idle_timeout`).
- Bus topics and log event names share one registry (`internal/model/registry.go`).
- Entity write + `event_log` append commit in one transaction (outbox).

Releases are cut by the trebi-pro pipeline. This repository carries the tags and the release assets.
