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

### mkdir
Make one empty folder, with its parent folders. The folder stays when it has no files. A path that exists returns `conflict`.
- `owner` (string, optional): `workspace` (default), `job`, or `skill`.
- `workspace` (string, required for owner `workspace`; optional for `job`): workspace id.
- `name` (string, required for owners `job` and `skill`): job or skill name.
- `path` (string, required): folder path relative to the owner root.

### move
Rename or move one file or one folder in the same owner. The daemon makes the parent folders of `to`. The reply is the new file info (`dir: true` for a folder).
- `owner` (string, optional): `workspace` (default), `job`, or `skill`.
- `workspace` (string, required for owner `workspace`; optional for `job`): workspace id.
- `name` (string, required for owners `job` and `skill`): job or skill name.
- `from` (string, required): current path relative to the owner root.
- `to` (string, required): new path relative to the owner root. A folder cannot move into itself.
- `overwrite` (boolean, optional): replace a file at `to`. Without it, a path at `to` returns `conflict`. A folder at `to` is never replaced.
- `if_match` (string, optional): current revision of a file `from`; a stale value returns 412.

### rmdir
Delete one folder and all the files in it. Destructive.
- `owner` (string, optional): `workspace` (default), `job`, or `skill`.
- `workspace` (string, required for owner `workspace`; optional for `job`): workspace id.
- `name` (string, required for owners `job` and `skill`): job or skill name.
- `path` (string, required): folder path relative to the owner root.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`, and the message gives the count of files that the delete removes.

### upload
Store one image so that a message can carry it. Give `path` or `content_base64`, not both. An upload is one PNG, JPEG, GIF, or WebP image; the daemon checks the type from the bytes. The reply is `{id, workspace_id, mime, size, sha256, created_at}`. Give the `id` in the `attachments` of `run` `message` or `assistant` `say`. A file that is not an image returns `unsupported_media_type`. A file above `uploads.max_bytes` (default 10 MB) returns `too_large`. An upload that no message uses goes away after 30 days.
- `workspace` (string, required): workspace id.
- `path` (string, optional): path of an image file in the workspace. A path outside the workspace returns `validation`.
- `content_base64` (string, optional): the image bytes, base64 encoded.

### upload_get
Read one stored image. The reply is `{"content": "…", "encoding": "base64"}`.
- `workspace` (string, required): workspace id.
- `id` (string, required): upload id.
- `max_bytes` (number, optional): cap the body at this size; default 64 KB.

## Related guides

`files`
