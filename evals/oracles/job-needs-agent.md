# Oracle: job-needs-agent

The oracle does not change state. It finds that the workspace has no agent. The answer to the user then asks to make an agent before the job.

```bash
trebi agent list --workspace ops --json | jq -e '(.agents // []) | length == 0'
trebi job list --json | jq -e '(.jobs // []) | length == 0'
```
