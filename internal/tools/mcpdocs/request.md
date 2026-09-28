# request

Request operations: a job run asks a person for a decision, then continues with the answer. Requests are a Pro feature. Without the license, `create` returns 402 `license_required`; answer, dismiss, and the reads still work.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message","details"}}` unchanged, with `isError: true`.

A request has one type. `confirm` asks for one action. `review` carries a draft that the person can change. `choose` lists options. `form` asks for a few fields. The person answers with one decision: `approve`, `edit`, `reject`, `respond`, `choose`, or `submit`. Trebi closes a request with no answer as `expired`.

In `resume` delivery (the default), end your turn after `create`. Do not wait and do not poll. Trebi stops the run and continues the same session when the person answers. The first prompt of the new run holds the answer in a fenced JSON block. In `poll` delivery the run stays alive and reads the answer with `get` and `wait`.

## Actions

### create
Open a request for the calling run. Only a job run can create one: the call needs a run token. The reply is `{"request": {...}, "next": "end_turn", "message"}` in resume delivery.
- `type` (string, required): `confirm`, `review`, `choose`, or `form`.
- `title` (string, required): one line that says what the person decides.
- `summary` (string, optional): Markdown context for the person.
- `target` (string, optional): what the action touches, for example a PR or a mail address.
- `draft` (object, optional): for `review`: `{"format": "text" | "markdown", "text": "..."}`.
- `options` (array, optional): for `choose`: `[{"id", "label", "detail"}]`.
- `multi` (boolean, optional): for `choose`: the person can pick more than one option.
- `schema` (object, optional): for `form`: a flat JSON Schema. Each property is a string (with optional `enum` or `format: multiline`), number, integer, or boolean. `required` is supported.
- `allow` (array, optional): narrow the decisions the person can give. A decision that the type does not support is refused.
- `report` (string, optional): an `.html` or `.md` file inside the run working dir. Trebi copies it at create time and shows it next to the decision. Maximum 5 MiB.
- `delivery` (string, optional): `resume` (default) or `poll`.
- `expires_in` (string, optional): a duration such as `24h`. The default is `requests.expire_after` (168h).

### get
Read one request. A run token reads only the requests of its own job.
- `id` (string, required): request id.
- `wait` (string, optional): a duration such as `30s`. The call returns when the request is closed or the time ends (maximum 60s).

### list
List requests, newest first. The reply is `{"requests": [...], "next_cursor"}`.
- `status` (string, optional): `pending`, `answered`, `expired`, `dismissed`, or `cancelled`.
- `run` (string, optional): run id.
- `job` (string, optional): job name.
- `workspace` (string, optional): workspace id.
- `cursor` (string, optional): the `next_cursor` of a prior page.
- `limit` (number, optional): page size.

### report
Read the report snapshot of a request as raw text.
- `id` (string, required): request id.
- `max_bytes` (number, optional): cut the body at this size.

### answer
Answer a pending request. A job run cannot answer. The assistant can, and the answer records `by: assistant`. A closed request returns 409 `request_closed` with the request in `details`.
- `id` (string, required): request id.
- `decision` (string, required): `approve`, `edit`, `reject`, `respond`, `choose`, or `submit`.
- `text` (string, optional): for `edit`: the changed draft. It must differ from the draft.
- `message` (string, optional): for `respond`: the reply to the job. Nothing is approved.
- `choices` (array, optional): for `choose`: option ids.
- `values` (object, optional): for `submit`: the form values.
- `note` (string, optional): a note to the job.
- `if_match` (string, optional): the request revision; a stale one returns 412.

### dismiss
Close a pending request with no answer. The run does not continue.
- `id` (string, required): request id.

## Related guides

`requests`
