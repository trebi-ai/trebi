# Oracle: remote-link

The seed started a second daemon and wrote URL/token under the workdir.

```bash
URL=$(cat "$EVAL_WORKDIR/remote.url")
TOKEN=$(cat "$EVAL_WORKDIR/remote.token")
echo "$TOKEN" | trebi workspace add --name remote --url "$URL" --token-stdin --use
trebi workspace list --json
trebi job list --json
```
