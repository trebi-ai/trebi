# Oracle: agent-root-job

The oracle does not change state. It finds the agents that can own the job. The answer to the user then asks which agent gets weekly-digest.

```bash
trebi agent list --workspace ops --json | jq -e '[.agents[] | select(.name == "cmo")] | length == 1'
trebi job list --json | jq -e '[.jobs[]? | select(.name | startswith("/"))] | length == 0'
```
