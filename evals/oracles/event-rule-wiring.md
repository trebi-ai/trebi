# Oracle: event-rule-wiring

```bash
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Ingest
harness: codex
triggers:
  - event: tick
    match: "true"
---
Ingest the event and stop.
EOF
trebi job delete ingest --force || true
trebi job create --file "$EVAL_WORKDIR/JOB.md" ingest
trebi job validate ingest
printf '{"n":1}\n' | trebi rules fire --integration tick --job ingest --match "true" --json
for _ in $(seq 1 20); do
  n=$(trebi runs list --job ingest --json | jq '(.runs // []) | length')
  [ "$n" -ge 1 ] && break
  sleep 2
done
trebi runs list --job ingest --json
```
