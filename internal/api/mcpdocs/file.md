# file

File operations: read and write files owned by a workspace, a job, or a skill.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

The `owner` field selects the file plane:
- `workspace`: set `workspace` to the workspace id.
- `job`: set `name` to the job name; `workspace` is optional.
- `skill`: set `name` to the skill name.

Content is text. Binary content is not supported over this tool.

## Actions

### list
List files of one owner.
- `owner` (string, optional): `workspace` (default), `job`, or `skill`.
- `workspace` (string, required for owner `workspace`; optional for `job`): workspace id.
- `name` (string, required for owners `job` and `skill`): job or skill name.
- `prefix` (string, optional): path prefix filter.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### get
Read one file, or its metadata.
- `owner` (string, optional): `workspace` (default), `job`, or `skill`.
- `workspace` (string, required for owner `workspace`; optional for `job`): workspace id.
- `name` (string, required for owners `job` and `skill`): job or skill name.
- `path` (string, required): file path relative to the owner root.
- `stat` (boolean, optional): return JSON metadata instead of content.

### put
Write one text file.
- `owner` (string, optional): `workspace` (default), `job`, or `skill`.
- `workspace` (string, required for owner `workspace`; optional for `job`): workspace id.
- `name` (string, required for owners `job` and `skill`): job or skill name.
- `path` (string, required): file path relative to the owner root.
- `content` (string, required): full file text.
- `content_type` (string, optional): media type; default inferred.
- `if_match` (string, optional): current revision; a stale value returns 412.

### remove
Delete one file. Destructive.
- `owner` (string, optional): `workspace` (default), `job`, or `skill`.
- `workspace` (string, required for owner `workspace`; optional for `job`): workspace id.
- `name` (string, required for owners `job` and `skill`): job or skill name.
- `path` (string, required): file path relative to the owner root.
- `if_match` (string, optional): current revision; a stale value returns 412.
