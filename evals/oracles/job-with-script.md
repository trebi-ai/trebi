# Oracle: job-with-script

```bash
cat > "$EVAL_WORKDIR/hello.sh" << 'EOF'
#!/bin/sh
echo script-ok > script-out.txt
EOF
chmod +x "$EVAL_WORKDIR/hello.sh"
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Scripter
harness: codex
---
Run ./hello.sh in this job folder, then stop.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent ops scripter
trebi job file put --from "$EVAL_WORKDIR/hello.sh" ops/scripter hello.sh
trebi job validate ops/scripter
trebi job run ops/scripter --test
trebi job run ops/scripter
for _ in $(seq 1 60); do
  n=$(trebi runs list --job ops/scripter --json | jq '[.runs[] | select(.status == "complete")] | length')
  [ "$n" -ge 1 ] && break
  sleep 5
done
```
