# Oracle: debug-failed-run

```bash
trebi runs list --status failed --json
trebi job show nightly --json
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Nightly
harness: codex
---
Write ok to nightly.txt in this job folder, then stop.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" nightly
trebi job validate nightly
trebi job run nightly --test
trebi job run nightly
for _ in $(seq 1 60); do
  n=$(trebi runs list --job nightly --json | jq '[.runs[] | select(.status == "complete")] | length')
  [ "$n" -ge 1 ] && break
  sleep 5
done
```
