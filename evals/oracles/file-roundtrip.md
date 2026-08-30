# Oracle: file-roundtrip

```bash
printf 'x,y\n3,4\n' > "$EVAL_WORKDIR/data.csv"
trebi file put --from "$EVAL_WORKDIR/data.csv" data.csv
trebi file list --json
trebi file get data.csv --out "$EVAL_WORKDIR/got.csv"
trebi file get data.csv --stat
hash=$(trebi file get data.csv --stat | jq -r .hash)
trebi file remove data.csv --if-match "$hash"
```
