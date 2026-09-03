# Oracle: drain-maintenance

```bash
trebi runs list --job sleeper --json | jq '[.runs[] | {id, status}]'
trebi service drain
trebi status --json | jq '.drain_state'
```
