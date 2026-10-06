# Oracle: job-memory-fix

```bash
trebi job run digest --wait --timeout 1m
trebi runs list --job digest --json | jq -e '[.runs[] | select(.status == "complete")] | length >= 1'
for i in 1 2 3 4 5 6 7 8 9 10; do
  n=$(trebi alerts --json | jq '[.alerts[] | select(.topic == "alert.raised" and .attrs.kind == "memory_invalid")] | length')
  if [ "$n" -ge 1 ]; then break; fi
  sleep 1
done
trebi alerts --json | jq -e '[.alerts[] | select(.topic == "alert.raised" and .attrs.kind == "memory_invalid")] | length >= 1'
trebi job memory log add --text "fake run ended memory_invalid; memory repaired" digest
trebi job memory log add --text "Next: run the digest again and check the memory gate." digest
trebi job memory show --json digest | jq -e '(.doc.log | length >= 2) and any(.doc.log[]; .text | startswith("Next:"))'
trebi job memory validate digest --json | jq -e '.valid == true and (.issues | length == 0)'
```
