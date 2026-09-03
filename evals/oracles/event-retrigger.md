# Oracle: event-retrigger

```bash
trebi event list --integration tick --json
id=$(trebi event list --integration tick --json | jq -r '.events[0].id')
trebi event retrigger "$id"
sleep 3
trebi runs list --job ingest --json
```
