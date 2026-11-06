# Oracle: event-retrigger

```bash
trebi event list --connector tick --json
id=$(trebi event list --connector tick --json | jq -r '.events[0].id')
trebi event retrigger "$id"
sleep 3
trebi runs list --job agents/ops/ingest --json
```
