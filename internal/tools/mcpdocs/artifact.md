# artifact

Artifact operations: save, read, version, send, and ask about the documents that agents, jobs, and the assistant make.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message","details"}}` unchanged, with `isError: true`.

An artifact is one named document in a workspace, for example a weekly report or an email draft. Each save makes a new numbered version. A version never changes. An artifact has a stable `id` (`art_` plus a ULID) and a `name` (lowercase letters, digits, and dashes). Each owner (the assistant, a job, or an agent) has its own names in a workspace. When two owners use the same name, use the id. Each action that takes `artifact` accepts the id, the name, or `name@n` for version `n`. A name needs `workspace`.

The `kind` sets how Trebi shows and sends the artifact: `markdown`, `email`, `html`, `table` (CSV), `json`, `code`, `svg`, `image`, `pdf`, or `file`. Trebi finds the kind from the file name when you do not give it.

An `email` artifact is a draft. Its content starts with a header block (`kind: email`, `from`, `to`, `cc`, `subject`, `in_reply_to`) between two `---` lines, then the body in Markdown. `from` is `trebi` (the email channel of the workspace) or the slug of a mailbox connection.

Inside a run, the artifact belongs to the job or agent of the run. Only the run that made an artifact, or a later run of the same owner, can save a new version. A save that does not start from the latest version fails with `stale` (409); `details.latest` gives the latest number. Read that version, merge, and save again with `base` set to it.

## Actions

### list
List the artifacts of a workspace, newest first. The reply is `{"items": [...], "next_cursor"}`. Each item has `id`, `name`, `title`, `kind`, `owner_kind`, `owner_id`, `live`, `latest_version`, `revision`, and `latest`.
- `workspace` (string, optional): workspace id or name.
- `owner` (string, optional): `assistant`, `job:<name>`, or `agent:<name>`.
- `kind` (string, optional): one kind.
- `live` (boolean, optional): only live artifacts, or with false, only the others.
- `q` (string, optional): text to find in the name or the title.
- `cursor` (string, optional): the `next_cursor` of a prior page.
- `limit` (number, optional): page size.

### get
Read one artifact with its latest version, `version_count`, and `delivery_count`.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.

### versions
List the versions of one artifact, newest first. Each version has `n`, `sha256`, `size_bytes`, `mime`, `file_name`, `note`, `origin_kind`, `run_id`, `author`, `base_n`, `pinned`, and `delivered`.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.

### version
Read the record of one version.
- `artifact` (string, required): artifact id, name, or `name@n`.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `version` (number, optional): version number. The default is the version in `name@n`, else the latest.

### content
Read the content of one version. Text comes back as is. Use it before you save a new version.
- `artifact` (string, required): artifact id, name, or `name@n`.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `version` (number, optional): version number. The default is the version in `name@n`, else the latest.
- `max_bytes` (number, optional): cap the body at this size; default 64 KB. A cut body returns `{"content": "…", "truncated": true}`.

### put
Save a new version. Give `name` for a new or existing artifact, or `artifact_id` for an existing one. A run saves to the artifact of that name of its job or agent. A person saves to the artifact of that name, or makes a new one for `owner`. Give the content in one of `content`, `content_base64`, or `from_path`. The reply is `{"artifact", "version"}`.
- `workspace` (string, optional): workspace id or name. A run uses its own workspace.
- `name` (string, optional): artifact name, for example `weekly-report`.
- `artifact_id` (string, optional): the id of an existing artifact.
- `title` (string, optional): the title that the user sees.
- `kind` (string, optional): the kind. The default comes from `file_name`, else from the content.
- `file_name` (string, optional): the file name of the content, for example `report.md`.
- `content` (string, optional): the content as text.
- `content_base64` (string, optional): the content as base64, for binary files.
- `from_path` (string, optional): a file path in the working folder of the run. Only a run can use it.
- `note` (string, optional): what changed in this version.
- `base` (number, optional): the version that this save starts from. A stale base fails with `stale`.
- `owner` (string, optional): `assistant`, `job:<name>`, or `agent:<name>`, for a new artifact outside a run.

### update
Change the title or the version limit of one artifact. The reply is the artifact.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `title` (string, optional): new title.
- `keep_versions` (number, optional): how many versions to keep. Trebi deletes older versions that are not pinned, live, or sent.
- `revision` (number, optional): the `revision` that you read. A different revision fails with `precondition_failed`.

### delete
Delete one artifact and every version of it. An agent run cannot delete an artifact.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required` and a message that names what it deletes.

### pin
Pin one version, so that the version limit never deletes it. With `off`, remove the pin. A run cannot pin.
- `artifact` (string, required): artifact id, name, or `name@n`.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `version` (number, optional): version number. The default is the version in `name@n`, else the latest.
- `off` (boolean, optional): remove the pin.

### send
Send an email draft. A draft with `from: trebi` goes through the email channel of the workspace. A draft with a mailbox connection goes through the `send` or `draft` operation of that connection. The operation can need the approval of the user; then the call fails with `approval_required` and `details.request_id`. Call again after the user approves. To send a report that is not a draft, use `email` `send` with `artifact`.
- `artifact` (string, required): artifact id, name, or `name@n` of an `email` artifact.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `version` (number, optional): version number. The default is the version in `name@n`, else the latest.
- `mode` (string, optional): `send` (the default) or `draft`, which saves the mail to the drafts of the mailbox.

### deliveries
List the mails that sent a version of this artifact.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.

### ask
Ask the maker of an artifact for a change. The text goes to the conversation, the run, or the assistant that made the version. A run cannot ask.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `text` (string, required): the request.
- `selection` (string, optional): the part of the content that the request is about.
- `version` (number, optional): the version that the request is about. The default is the latest.

### run_artifacts
List the versions that one run saved.
- `run_id` (string, required): run id.

## Related guides

`artifacts`
