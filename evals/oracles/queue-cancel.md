# Oracle: queue-cancel

```bash
trebi queue --json
id=$(trebi queue --json | jq -r '(.items // [])[] | select(.status == "pending") | .id' | head -1)
trebi queue cancel "$id"
trebi queue --json
```
