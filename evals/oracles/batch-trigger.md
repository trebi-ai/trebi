# Oracle: batch-trigger

```bash
trebi guide jobs-and-scheduling
trebi job create digest --workspace ops --prompt "Summarize the new tick events in a few lines." --on tick --batch 15m --harness codex
trebi job validate digest --workspace ops --json
trebi job show digest --workspace ops --json | jq '.triggers'
```
