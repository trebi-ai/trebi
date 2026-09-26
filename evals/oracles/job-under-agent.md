# Oracle: job-under-agent

```bash
trebi job create weekly-digest --agent cmo --prompt "Draft the weekly growth digest. Write it to digest.txt."
trebi job list --json | jq -e '[.jobs[] | select(.name == "agents/cmo/weekly-digest" and .owner == "agents/cmo")] | length == 1'
trebi agent wake cmo --test | jq -e '.prompt_preview | contains("# Your jobs") and contains("weekly-digest")'
```
