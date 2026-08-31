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
- `provenance` (string, optional): origin of the skill, one of `local`,
  `assistant`, `git`, `registry`, `integration`. Leave it unset unless you know
  the skill came from elsewhere: it defaults to `local`, and only `local` and
  `assistant` skills accept support-file writes. A skill created with any other
  provenance rejects `file put` with `conflict`, and the fix is to create the
  skill again as `local`.

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

## Related guides

`skills-secrets-config`
