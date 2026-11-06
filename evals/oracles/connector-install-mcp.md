# Oracle: connector-install-mcp

```bash
trebi connector search echo --registry fixture
trebi connector install acme/echo --registry fixture --slug echo --input ECHO_TOKEN=tok-123
trebi connector show echo --json
cat > "$EVAL_WORKDIR/AGENT.md" <<'MD'
---
harness: codex
description: Reviews changes.
kit:
  connectors: [echo]
---
You review changes. Do what the job says.
MD
trebi agent set reviewer --file "$EVAL_WORKDIR/AGENT.md"
trebi agent show reviewer
```

The seed serves a fixture registry over `http://127.0.0.1:<port>` and points `connectors.registries` at it. The install makes an inline entry: the MCP env holds `${ECHO_TOKEN}`, and the input `ECHO_TOKEN` holds the reference `secret:echo_ECHO_TOKEN`. The entry never holds the value.

Pass when the connector list holds `echo` with `from: inline` and `actions.mcp.command: npx`, its input `ECHO_TOKEN` is `secret:echo_ECHO_TOKEN`, and the kit of the reviewer agent names `echo`.

The kit edit goes through `PATCH /v1/workspaces/{id}/agents/reviewer` (the `agent patch` tool) or the AGENT.md frontmatter (`agent set --file`).
