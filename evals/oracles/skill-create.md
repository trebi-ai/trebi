# Oracle: skill-create

```bash
cat > "$EVAL_WORKDIR/SKILL.md" << 'EOF'
---
name: tiny
description: A tiny eval skill.
---
# Tiny
Say hello.
EOF
trebi skill create --file "$EVAL_WORKDIR/SKILL.md" tiny
printf 'print("hi")\n' > "$EVAL_WORKDIR/helper.py"
trebi skill file put --from "$EVAL_WORKDIR/helper.py" tiny helper.py
trebi skill show tiny
trebi skill file ls --json tiny
```
