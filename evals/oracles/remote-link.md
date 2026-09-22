# Oracle: remote-link

The seed started a second daemon and wrote its URL and workspace id under the
workdir.

```bash
URL=$(cat "$EVAL_WORKDIR/remote.url")
WS=$(cat "$EVAL_WORKDIR/remote.workspace")
trebi workspace create --url "$URL" --id "$WS" --name remote --use
trebi workspace list --json
trebi job list --json
```
