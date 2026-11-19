# Oracle: drain-maintenance

```bash
trebi runs list --job ops/sleeper --json | jq '[.runs[] | {id, status}]'
trebi service drain
trebi status --json | jq '.drain_state'
```
