# Oracle: onboarding-assistant

```bash
trebi workspace create --name ops --mkdir --use "$EVAL_WORKDIR/ops"
cat > "$EVAL_WORKDIR/assistant-context.md" << 'EOF'
You are the workspace assistant. Answer briefly.
EOF
trebi assistant set --workspace ops --context-file "$EVAL_WORKDIR/assistant-context.md"
trebi assistant show --workspace ops
trebi assistant say "hello, what workspace is this?" --wait 120000 --json
```
