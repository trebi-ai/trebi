# Oracle: onboarding-first-job

```bash
trebi workspace add --name ops --create --use "$EVAL_WORKDIR/ops"
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Hello
harness: codex
---
Write the current UTC date to out.txt in this job's folder, then stop.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" hello
trebi job validate hello
trebi job run hello --test
trebi job run hello
for _ in $(seq 1 60); do
  n=$(trebi runs list --job hello --json | jq '[.runs[] | select(.status == "complete")] | length')
  [ "$n" -ge 1 ] && break
  sleep 5
done
```
