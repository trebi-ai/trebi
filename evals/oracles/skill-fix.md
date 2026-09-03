# Oracle: skill-fix

```bash
trebi skill file get tiny helper.py
printf 'print("hello")\n' > "$EVAL_WORKDIR/helper.py"
trebi skill file put --from "$EVAL_WORKDIR/helper.py" tiny helper.py
trebi skill file get tiny helper.py
```
