# Oracle: job-schedule

```bash
trebi job schedule ops/report "0 7 * * *"
trebi job pause ops/report
trebi job show ops/report --json | jq '.triggers, .active'
```
