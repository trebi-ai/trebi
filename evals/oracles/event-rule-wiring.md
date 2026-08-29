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
```
