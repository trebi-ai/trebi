# Oracle: job-unpause

```bash
trebi job show ops/report --json | jq '.active, .triggers'
trebi job unpause ops/report
trebi job show ops/report --json | jq '.active, .triggers'
```
