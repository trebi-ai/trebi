# Oracle: onboarding-first-workspace

```bash
trebi workspace add --name ops --create --use "$EVAL_WORKDIR/ops"
trebi workspace list --json
trebi workspace current --json
test -f "$(trebi workspace current --json | jq -r .path)/trebi.yaml"
```
