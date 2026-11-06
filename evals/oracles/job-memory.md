# Oracle: job-memory

```bash
trebi job memory validate agents/ops/report --json | jq -e '.valid == true and .legacy == false'
trebi job memory lesson add --text "Shipped v1" --at 2026-09-12 agents/ops/report
trebi job memory lesson add --text "Pager rotation on Fridays" agents/ops/report
trebi job memory show --json agents/ops/report | jq -e '[.doc.lessons[].text] | join(" ") | test("Shipped v1") and test("Pager rotation")'
trebi job memory log add --text "recorded two lessons" agents/ops/report
trebi job memory validate agents/ops/report --json | jq -e '.valid == true and (.issues | length == 0)'
```
