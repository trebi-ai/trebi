# Oracle: agent-skill-scope

```bash
trebi agent wake cmo --workspace ops --wait --timeout 8m --extra-prompt "Make a trebi skill named tone for yourself only. Its SKILL.md has a frontmatter description and the line: Write in a playful voice. Use trebi skill create with no scope flag, so the skill goes in your own scope. Do not change the global tone skill."
trebi skill list --effective-for cmo --workspace ops --json | jq -e '[.skills[] | select(.name == "tone" and .used == true and .scope == "agent")] | length == 1'
trebi skill show tone --scope global
```
