# Oracle: run-retry

```bash
id=$(trebi runs list --job agents/ops/nightly --json | jq -r '.runs[0].id')
trebi runs retry "$id"
trebi runs list --job agents/ops/nightly --json
```
