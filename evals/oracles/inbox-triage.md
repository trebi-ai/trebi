# Oracle: inbox-triage

```bash
trebi inbox read --all
run=$(trebi runs approvals --state pending --json | jq -r '.approvals[0].run_id // empty')
if [ -n "$run" ]; then
  trebi runs approve "$run" --decision allow
fi
trebi inbox ls --unread --json
trebi runs approvals --state decided --json
```
