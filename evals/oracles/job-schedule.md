# Oracle: job-schedule

```bash
trebi job schedule agents/ops/report "0 7 * * *"
trebi job pause agents/ops/report
trebi job show agents/ops/report --json | jq '.triggers, .active'
```
