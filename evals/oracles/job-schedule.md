# Oracle: job-schedule

```bash
trebi job schedule report "0 7 * * *"
trebi job pause report
trebi job show report --json | jq '.triggers, .active'
```
