# file

File operations: read and write files owned by a workspace, a job, or a skill.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

The `owner` field selects the file plane:
- `workspace`: set `workspace` to the workspace id.
- `job`: set `name` to the job name; `workspace` is optional.
- `skill`: set `name` to the skill name. Only skills whose provenance is `local`
  or `assistant` accept writes; any other provenance returns `conflict`.
  `SKILL.md` itself is not writable here — use the `skill` tool `set` action.

Content is text by default. For a binary file, `put` takes `encoding: "base64"`,
and `get` returns `{"content": "…", "encoding": "base64"}`.

## Actions

### list
List files of one owner.
- `owner` (string, optional): `workspace` (default), `job`, or `skill`.
- `workspace` (string, required for owner `workspace`; optional for `job`): workspace id.
- `name` (string, required for owners `job` and `skill`): job or skill name.
- `prefix` (string, optional): path prefix filter.
- `recursive` (boolean, optional): add a row per folder, marked `dir: true`, so the tree shape is visible. Files are always listed at full depth.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### get
Read one file, or its metadata.
- `owner` (string, optional): `workspace` (default), `job`, or `skill`.
- `workspace` (string, required for owner `workspace`; optional for `job`): workspace id.
- `name` (string, required for owners `job` and `skill`): job or skill name.
- `path` (string, required): file path relative to the owner root.
- `stat` (boolean, optional): return JSON metadata instead of content.
- `max_bytes` (number, optional): cap the body at this size; default 64 KB. A cut body returns `{"content": "…", "truncated": true}`.
- `encoding` (string, optional): a binary file comes back as `{"content": "…", "encoding": "base64"}`; text comes back as text.

### put
Write one text file.
- `owner` (string, optional): `workspace` (default), `job`, or `skill`.
- `workspace` (string, required for owner `workspace`; optional for `job`): workspace id.
- `name` (string, required for owners `job` and `skill`): job or skill name.
- `path` (string, required): file path relative to the owner root.
- `content` (string, required): full file text.
- `if_match` (string, optional): current revision; a stale value returns 412.
- `encoding` (string, optional): `text` (default) or `base64` for binary content.

### delete
Delete one file. Destructive.
- `owner` (string, optional): `workspace` (default), `job`, or `skill`.
- `workspace` (string, required for owner `workspace`; optional for `job`): workspace id.
- `name` (string, required for owners `job` and `skill`): job or skill name.
- `path` (string, required): file path relative to the owner root.
- `if_match` (string, optional): current revision; a stale value returns 412.
- `confirm` (boolean, required for a workspace file): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

## Related guides

`files`
