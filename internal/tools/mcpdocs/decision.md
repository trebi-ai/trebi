# decision

Safety checks that Trebi makes before it acts: list, read, test, and give feedback.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

A decision asks a model fixed questions about one piece of content, for example a reply to an agent mail. A policy maps the answers to an outcome. The first kind is `email.reply_risk`: the outcome `deliver` gives the reply to the agent, and `hold` keeps it in the Inbox for the user. A check that does not finish (`status` other than `ok`) gives the safe outcome of the kind, which is `hold` for mail. Each decision stores its questions, its answers with a confidence, and its reasons. Show the user the kind label (for example "Email reply check"), not the kind id.

## Actions

### list
List decisions, newest first. The reply carries `items` and, when more rows exist, `next_cursor`.
- `workspace` (string, optional): workspace id or name.
- `kind` (string, optional): decision kind, for example `email.reply_risk`.
- `outcome` (string, optional): for example `deliver` or `hold`.
- `status` (string, optional): `ok`, `invalid`, `error`, `timeout`, or `off`.
- `subject_kind` (string, optional): the kind of object that the decision is about, for example `email`.
- `subject_id` (string, optional): the id of that object, for example a message id.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size, at most 200.

### get
Read one decision: the questions, the answers, the reasons, the decider, the time, and the feedback.
- `id` (string, required): decision id.

### raw
Read the raw output of the decider for one decision. Use it only to find why a check failed.
- `id` (string, required): decision id.

### kinds
List the decision kinds: label, version, questions, safe outcome, and the decider that each kind uses now. No input fields.

### stats
Count the decisions of a time window: totals per outcome and per status, the feedback count, the agree rate, and the p50 and p95 time.
- `workspace` (string, optional): workspace id or name.
- `kind` (string, optional): decision kind.
- `since` (string, optional): RFC3339 time or a duration such as `168h`. The default is 30 days.

### test
Run a check on a text and store nothing. Use it when the user asks "Try the email check on this text". Only a person can run a test; when you call it for the user, the call fails with 403.
- `kind` (string, required): decision kind.
- `text` (string, required): the text to check.
- `facts` (object, optional): trusted context as string values, for example `{"agent": "research", "can_send": "true"}`.

### feedback
Tell Trebi if a decision was right. Only a person can give feedback. A release of a held reply records `disagree`, and an ignore records `agree`.
- `id` (string, required): decision id.
- `value` (string, required): `agree` or `disagree`.

## Related guides

`inbox-and-alerts`
