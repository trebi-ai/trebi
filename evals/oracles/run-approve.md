# Oracle: run-approve

```bash
trebi runs approvals --state pending --json
run=$(trebi runs approvals --state pending --json | jq -r '.approvals[0].run_id')
trebi runs approve "$run" --decision allow
trebi runs approvals --state pending --json
trebi runs approvals --state decided --json
```
