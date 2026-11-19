# Oracle: event-rule-wiring

```bash
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Ingest
harness: codex
---
Ingest the event and stop.
EOF
trebi job delete agents/ops/ingest --force || true
trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent ops ingest
mkdir -p "$EVAL_WORKDIR/ops/routes"
cat > "$EVAL_WORKDIR/ops/routes/tick.yaml" << 'EOF'
routes:
  - job: ops/ingest
    gets: every_message
    match: "true"
EOF
trebi job validate agents/ops/ingest
printf '{"n":1}\n' | trebi rules fire --connector tick --job agents/ops/ingest --match "true" --json
for _ in $(seq 1 20); do
  n=$(trebi runs list --job agents/ops/ingest --json | jq '(.runs // []) | length')
  [ "$n" -ge 1 ] && break
  sleep 2
done
trebi runs list --job agents/ops/ingest --json
```
