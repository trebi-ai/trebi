# Oracle: onboarding-assistant

```bash
trebi workspace add --name ops --create --use "$EVAL_WORKDIR/ops"
cat > "$EVAL_WORKDIR/TREBI.md" << 'EOF'
---
harness: codex
---
You are the workspace assistant. Answer briefly.
EOF
trebi file put --from "$EVAL_WORKDIR/TREBI.md" TREBI.md
trebi assistant show
trebi assistant say "hello, what workspace is this?" --json
```
