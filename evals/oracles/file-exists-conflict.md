# Oracle: file-exists-conflict

```bash
hash=$(trebi file get --stat data.csv | jq -r .hash)
printf 'a,b\n9,9\n' > "$EVAL_WORKDIR/data.csv"
trebi file put --from "$EVAL_WORKDIR/data.csv" --if-match "$hash" data.csv
```
