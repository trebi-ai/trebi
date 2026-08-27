---
# important comment
working_dir: .
command: claude
schedule: "0 9 * * *"
env: claude-zai
session:
  timeout: 30m
on:
  - source: whatsapp
    match: "true"
---
Do the thing. JOBI_TASK_COMPLETE
