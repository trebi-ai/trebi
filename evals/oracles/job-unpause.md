# Oracle: job-unpause

```bash
trebi job show agents/ops/report --json | jq '.active, .triggers'
trebi job unpause agents/ops/report
trebi job show agents/ops/report --json | jq '.active, .triggers'
```
