# Oracle: onboarding-assistant

```bash
trebi workspace create --name ops --mkdir --use "$EVAL_WORKDIR/ops"
cat > "$EVAL_WORKDIR/TREBI.md" << 'EOF'
---
harness: codex
---
You are the workspace assistant. Answer briefly.
EOF
trebi file put --from "$EVAL_WORKDIR/TREBI.md" TREBI.md
trebi assistant show
trebi assistant say "hello, what workspace is this?" --wait 120000 --json
```
