# Oracle: onboarding-first-job

```bash
trebi agent create helper --workspace ops --harness codex --description "Runs small chores." --prompt "You run small chores. Do what the job says."
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Hello
harness: codex
---
Write the current UTC date to out.txt in this job's folder, then stop.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent helper hello
trebi job validate agents/helper/hello
trebi job run agents/helper/hello --test
trebi job run agents/helper/hello
for _ in $(seq 1 60); do
  n=$(trebi runs list --job agents/helper/hello --json | jq '[.runs[] | select(.status == "complete")] | length')
  [ "$n" -ge 1 ] && break
  sleep 5
done
```
