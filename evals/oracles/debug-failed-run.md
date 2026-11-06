# Oracle: debug-failed-run

```bash
trebi runs list --status failed --json
trebi job show agents/ops/nightly --json
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Nightly
harness: codex
---
Write ok to nightly.txt in this job folder, then stop.
EOF
trebi job set --file "$EVAL_WORKDIR/JOB.md" agents/ops/nightly
trebi job validate agents/ops/nightly
trebi job run agents/ops/nightly --test
trebi job run agents/ops/nightly
for _ in $(seq 1 60); do
  n=$(trebi runs list --job agents/ops/nightly --json | jq '[.runs[] | select(.status == "complete")] | length')
  [ "$n" -ge 1 ] && break
  sleep 5
done
```
