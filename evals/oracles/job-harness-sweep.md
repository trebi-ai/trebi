# Oracle: job-harness-sweep

```bash
for name in alpha beta gamma; do
  cat > "$EVAL_WORKDIR/JOB.md" << EOF2
---
title: $name
harness: codex
---
Write $name.txt with one line, then stop.
EOF2
  trebi job set --file "$EVAL_WORKDIR/JOB.md" "agents/ops/$name"
  trebi job validate "agents/ops/$name"
done
trebi job list --json
```
