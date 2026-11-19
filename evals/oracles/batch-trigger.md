# Oracle: batch-trigger

```bash
trebi guide jobs-and-scheduling
trebi job create digest --workspace ops --agent ops --prompt "Summarize the new tick events in a few lines." --harness codex
mkdir -p "$EVAL_WORKDIR/ops/routes"
cat > "$EVAL_WORKDIR/ops/routes/tick.yaml" << 'EOF'
routes:
  - job: ops/digest
    gets: every_message
    batch: 15m
EOF
trebi job validate ops/digest --workspace ops --json
```
