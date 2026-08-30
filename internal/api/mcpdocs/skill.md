# skill

Skill operations: manage reusable skill documents.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List skills.
- `limit` (number, optional): page size.

### show
Read one skill.
- `name` (string, required): skill name.

### create
Create one skill.
- `name` (string, required): skill name.
- `content` (string, required): skill document text.
- `provenance` (string, optional): origin label.

### set
Replace one skill document.
- `name` (string, required): skill name.
- `content` (string, required): full skill document text.

### update
Refresh one skill from its source.
- `name` (string, required): skill name.

### delete
Remove one skill. Destructive.
- `name` (string, required): skill name.
