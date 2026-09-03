# Oracle: inbox-triage

```bash
ids=$(trebi inbox list --status unread --json | jq -r '.items[]?.id // empty')
for id in $ids; do
  trebi inbox resolve "$id"
done
run=$(trebi runs approvals --state pending --json | jq -r '.approvals[0].run_id // empty')
if [ -n "$run" ]; then
  trebi runs approve "$run" --decision allow
fi
trebi inbox list --status unread --json
trebi runs approvals --state decided --json
```
