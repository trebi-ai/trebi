# Oracle: inbox-triage

```bash
trebi inbox list --status unread --json
ids=$(trebi inbox list --status unread --json | jq -r '.items[].id // .items[]?.id // empty')
for id in $ids; do
  trebi inbox resolve "$id"
done
trebi inbox list --status unread --json
```
