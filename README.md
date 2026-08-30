# trebi

Trebi is the successor to jobi: an LLM task runner with an HTTP `/v1` API, SSE event bus, SQLite store, and `trebi` CLI.

This tree implements Phase 2 of the migration plan ([docs/trebi/backend-jobi-migration-0808.md](../docs/trebi/backend-jobi-migration-0808.md)) against the contract in [docs/trebi/backend-0808.md](../docs/trebi/backend-0808.md).

## Layout

```
trebi/
  cmd/trebi/           # single binary (CLI + daemon)
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
  scripts/
    eval.py                    # uv: run/oracle/review + report/ls/why/compare
    migrate-jobi-to-trebi.py   # uv migration (dry-run default)
```

## Build & test

```bash
cd trebi
go test ./...
go build -o ../build/trebi ./cmd/trebi
./../build/trebi --help
```

## Daemon (temp home)

```bash
export TREBI_HOME=/tmp/trebi-dev
./build/trebi service migrate-db
./build/trebi service daemon --listen 127.0.0.1:7433
# other terminal
./build/trebi service status
./build/trebi runs list
```

## Migration (dry-run only until you cut over)

```bash
uv run scripts/migrate-jobi-to-trebi.py status
uv run scripts/migrate-jobi-to-trebi.py migrate --shadow
uv run scripts/migrate-jobi-to-trebi.py rewrite-jobs --diff
uv run scripts/migrate-jobi-to-trebi.py verify
```

**Never pass `--apply` until you are ready for cutover.** The script refuses `--apply` when the goal guard is active and defaults to dry-run plan output.

## Conventions

- Names: run, workspace, integration, harness (not task/source/tool).
- Canonical frontmatter only (`harness`, `triggers`, `envset`, `session.idle_timeout`).
- Bus topics and log event names share one registry (`internal/model/registry.go`).
- Entity write + `event_log` append commit in one transaction (outbox).
