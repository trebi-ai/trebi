# Oracle: alerts-window

```bash
since=$(cat "$EVAL_WORKDIR/alerts-since.txt" 2>/dev/null || date -u +%Y-%m-%dT00:00:00Z)
trebi alerts --since "$since" --json
```
