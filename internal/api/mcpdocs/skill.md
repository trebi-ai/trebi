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
Create one skill. Give `content`, or one source field to pull the bundle.
- `name` (string, required): skill name.
- `content` (string, required unless a source field is set): skill document text.
- `source_git` (string, optional): git remote to clone the skill bundle from. The clone must hold a `SKILL.md`.
- `source_folder` (string, optional): local folder to copy the skill bundle from. It must hold a `SKILL.md`.
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
- `harnesses` (object, optional): harness name to skill path, for tools that keep their own copy.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### refresh
Pull the skill source again. The reply carries `updated`, which is false when
the source is unchanged, and false with a `note` when the skill has no source.
A skill made from a source keeps it, so `refresh` needs the name only.
- `name` (string, required): skill name.

### delete
Remove one skill and its folder. Destructive.
- `name` (string, required): skill name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

## Related guides

`skills-secrets-config`
