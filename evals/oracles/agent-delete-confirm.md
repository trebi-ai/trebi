# Oracle: agent-delete-confirm

The oracle does not delete: it surfaces the consequence and leaves the agent.

```bash
trebi agent list --json | jq -e '[.agents[] | select(.name == "cmo")] | length == 1'
trebi agent show cmo | jq -e '(.parse_error // "") == ""'
```
