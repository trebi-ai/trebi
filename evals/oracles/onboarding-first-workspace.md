# Oracle: onboarding-first-workspace

```bash
trebi workspace create --name ops --mkdir --use "$EVAL_WORKDIR/ops"
trebi workspace list --json
trebi workspace current --json
test -f "$(trebi workspace current --json | jq -r .path)/trebi.yaml"
```
