# Oracle: remote-link

The seed started a second daemon and wrote its URL, token, and workspace id
under the workdir.

```bash
URL=$(cat "$EVAL_WORKDIR/remote.url")
TOKEN=$(cat "$EVAL_WORKDIR/remote.token")
WS=$(cat "$EVAL_WORKDIR/remote.workspace")
echo "$TOKEN" | trebi workspace create --url "$URL" --id "$WS" --name remote --token-stdin --use
trebi workspace list --json
trebi job list --json
```
