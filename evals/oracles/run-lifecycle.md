# Oracle: run-lifecycle

```bash
trebi job run agents/ops/sleeper
sleep 3
id=$(trebi runs list --job agents/ops/sleeper --json | jq -r '.runs[0].id')
trebi runs message "$id" "please stop after this message"
sleep 1
trebi runs kill "$id"
```
