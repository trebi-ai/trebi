# Oracle: secret-flow

```bash
trebi secret set --from-file "$EVAL_WORKDIR/api-key.txt" eval_api_key
cat > "$EVAL_WORKDIR/JOB.md" << 'EOF'
---
title: Secret user
harness: codex
env:
  EVAL_API_KEY: eval_api_key
---
Print the names of environment variables that start with EVAL_, never their values. Stop.
EOF
trebi job create --file "$EVAL_WORKDIR/JOB.md" --agent ops secret-user
trebi job validate agents/ops/secret-user
trebi job run agents/ops/secret-user --test
trebi job run agents/ops/secret-user
for _ in $(seq 1 60); do
  n=$(trebi runs list --job agents/ops/secret-user --json | jq '[.runs[] | select(.status == "complete" or .status == "failed")] | length')
  [ "$n" -ge 1 ] && break
  sleep 5
done
```
