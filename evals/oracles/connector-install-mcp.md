# Oracle: connector-install-mcp

```bash
trebi connector search echo --registry fixture
trebi connector install acme/echo --as echo --env ECHO_TOKEN=tok-123
trebi connector show echo
trebi agent show reviewer
```

The seed serves a fixture registry over `http://127.0.0.1:<port>` and points
`connectors.registries` at it. The install writes `ECHO_TOKEN` as the secret
`echo_ECHO_TOKEN` and refers to it from the MCP env, so the definition holds a
reference, never the value.

Pass when the connector list holds `echo` with `source.registry: fixture` and
`actions.mcp.command: npx`, its `ECHO_TOKEN` is `secret:echo_ECHO_TOKEN`, and
the reviewer agent's kit names `echo`.

The kit edit goes through `PATCH /v1/workspaces/{id}/agents/reviewer` (the
`agent patch` tool) or the AGENT.md frontmatter.
