# Oracle: workspace-rename

```bash
ws=$(trebi workspace list --json | jq -r '.workspaces[] | select(.name == "ops") | .id')
trebi workspace set "$ws" --name platform-ops --git-enabled
trebi workspace show "$ws" --json
trebi workspace list --json
```
