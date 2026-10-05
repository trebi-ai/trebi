# Oracle: agent-create-wake

```bash
trebi agent create cmo --harness codex --prompt "You watch growth. Answer in one line."
trebi agent show cmo | jq -e '(.parse_error // "") == ""'
trebi agent wake cmo --test | jq -e '.prompt_preview | contains("You watch growth")'
trebi agent wake cmo --wait --timeout 2m
trebi runs list --kind agent --json | jq -e '[.runs[] | select(.kind == "agent")] | length >= 1'
```
