# Oracle: run-lifecycle

```bash
trebi job run sleeper
sleep 3
id=$(trebi runs list --job sleeper --json | jq -r '.runs[0].id')
trebi runs message "$id" "please stop after this message"
sleep 1
trebi runs kill "$id"
```
