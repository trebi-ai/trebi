# artifact

Artifact operations: save, read, version, send, and ask about the documents that agents, jobs, and the assistant make.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message","details"}}` unchanged, with `isError: true`.

An artifact is one named document in a workspace, for example a weekly report or an email draft. Each save makes a new numbered version. A version never changes. An artifact has a stable `id` (`art_` plus a ULID) and a `name` (lowercase letters, digits, and dashes). Each owner (the assistant, a job, or an agent) has its own names in a workspace. When two owners use the same name, use the id. Each action that takes `artifact` accepts the id, the name, or `name@n` for version `n`. A name needs `workspace`.

The `kind` sets how Trebi shows and sends the artifact: `markdown`, `email`, `html`, `table` (CSV), `json`, `code`, `svg`, `image`, `pdf`, or `file`. Trebi finds the kind from the file name when you do not give it.

An `email` artifact is a draft. Its content starts with a header block (`kind: email`, `from`, `to`, `cc`, `subject`, `in_reply_to`) between two `---` lines, then the body in Markdown. `from` is `trebi` (the email channel of the workspace) or the slug of a mailbox connection.

A live artifact is a Markdown or HTML template plus data sources. A source is a read-only operation of a connection, with fixed input. A refresh runs the sources with no AI model and stores the result as a snapshot. A Markdown template uses Go template syntax on the data, for example `{{ table .sales }}`; an HTML template reads `window.trebi.data`. When a version adds or changes a source, the user must allow it first. Until then, a refresh uses the last allowed version.

Inside a run, the artifact belongs to the job or agent of the run. Only the run that made an artifact, or a later run of the same owner, can save a new version. A save that does not start from the latest version fails with `stale` (409); `details.latest` gives the latest number. Read that version, merge, and save again with `base` set to it.

## Actions

### list
List the artifacts of a workspace, newest first. The reply is `{"items": [...], "next_cursor"}`. Each item has `id`, `name`, `title`, `kind`, `owner_kind`, `owner_id`, `live`, `latest_version`, `conversation_id` (the chat of the artifact, when it has one), `revision`, and `latest`.
- `workspace` (string, optional): workspace id or name.
- `conversation` (string, optional): only the artifacts whose chat is this conversation, or that have a version that this conversation saved.
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
- `snapshot` (string, optional): for a live artifact, the snapshot whose data fills the template. The default is the newest snapshot.
- `raw` (boolean, optional): for a live artifact, read the template with no data.
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
- `live` (object, optional): makes the content a live template (kind `markdown` or `html`). The object is `{"params": [...], "sources": [...]}`. Each source is `{"name", "connection", "operation", "input"}`; `name` is lowercase letters, digits, and `_`. Each parameter is `{"name", "label", "type", "options", "values", "default"}`; `type` is `enum`, `date`, or `number`, and `values` maps an enum option to the value that the source gets. Put a parameter into a source input only as a whole value, such as `"{param.period}"`. A new version with no `live` keeps the sources of the latest version. The reply `version.approval` is `approved` or `pending`.

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

### refresh
Run the sources of a live artifact and store a new snapshot. The reply is the snapshot: `id`, `version`, `inputs`, `errors` (per source), and `took_ms`. A source that fails keeps its error text; the data of the other sources stays. With no allowed version, the call fails with `pending_approval` (409). An agent can refresh one artifact once a minute; a sooner call fails with `rate_limited` (429) and `details.retry_after_ms`.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `inputs` (object, optional): parameter name to value, for example `{"period": "30d"}`. A parameter that you do not give uses its default.

### snapshots
List the snapshots of a live artifact, newest first.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `limit` (number, optional): page size; default 50.

### snapshot
Read the record of one snapshot.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `snapshot` (string, required): snapshot id.

### snapshot_data
Read the data of one snapshot: `{"snapshot_id", "sources": {"<name>": {"rows": {"columns", "values"}, "json", "text", "error"}}}`.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `snapshot` (string, required): snapshot id.
- `max_bytes` (number, optional): cap the body at this size; default 64 KB.

### decide
Allow or deny the data sources of a pending version. An allowed version becomes the live version. Only the user can decide; an agent gets `forbidden`.
- `artifact` (string, required): artifact id, name, or `name@n`.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `version` (number, optional): version number. The default is the version in `name@n`, else the latest.
- `decision` (string, required): `approve` or `deny`.

### schedule
Refresh a live artifact on a schedule, and send each snapshot by email. With `off`, remove the schedule. Only the user can set a schedule; an agent gets `forbidden`.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `schedule` (string, optional): `@every 24h`, or a cron line such as `0 9 * * 1`. Required unless `off`.
- `timezone` (string, optional): the time zone of a cron line, for example `Europe/Berlin`. The default is UTC.
- `email_to` (array, optional): send each snapshot to these addresses.
- `subject` (string, optional): the subject of the mail. The default is the title.
- `off` (boolean, optional): remove the schedule.

### deliveries
List the mails that sent a version of this artifact.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.

### ask
Ask the owner of an artifact for a change. The text goes to the chat of the artifact as a message of the user, the same chat that `chat` opens. The reply stays in that chat. The reply is `{"via": "conversation", "conversation_id"}`. A run cannot ask.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `text` (string, required): the request.
- `selection` (string, optional): the part of the content that the request is about.
- `version` (number, optional): the version that the request is about. The default is the latest.

### chat
Open the one chat of an artifact. The chat goes to the owner: the conversation that saved the newest version, else a new conversation with the agent that owns the artifact, else a new conversation with the workspace assistant. The first call stores the chat on the artifact; later calls give the same chat. A new chat starts with one line that names the artifact and its version. The reply is `{"conversation_id", "created"}`. A run cannot open a chat.
- `artifact` (string, required): artifact id or name.
- `workspace` (string, optional): workspace id or name. Required with a name.
- `version` (number, optional): the version that the chat is about. The default is the latest.

### run_artifacts
List the versions that one run saved.
- `run_id` (string, required): run id.

## Related guides

`artifacts`
