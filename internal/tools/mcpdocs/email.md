# email

Email channel operations: status, test message, workspace addresses, the envelope log, held replies, and the inbound poll.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

The channel is off until the operator configures the `email` block in the daemon config. `status` works with the block absent and reports why the channel is off. A workspace address is masked unless you pass `reveal`; the full address is a secret the operator decides to show.

## Actions

### status
Read the email channel state: configured flag, worker and domain, admin list (`admins`), poll interval, DMARC policy, entitlement, address count, and the last poll result. Works with the block absent; then `configured` is false and the reply names the cause. No input fields.

### test
Send one fixed test message to the admins (`email.admins`). Use it after you change the config, to prove the path to the mailbox works. The reply carries `message_id`. No input fields.

### send
Send one explicit message from the workspace address, or from the agent subaddress when `agent` is set. The reply carries `message_id`. Recipient rule: every address in `to` must pass one of these. It is an admin or on the workspace allowlist. It is on the `allowlist` of an `event: email` trigger of the same job. The owner agent of the job accepts it in `email.accept`. Anything else answers 400 `validation` with `details.recipient` and `details.fix`, and opens one inbox item per sender and address. `details.fix` lists the changes that open the gate of the sender. Ask the user to allow the address before you send to a third party.
- `workspace` (string, required): workspace id or name.
- `agent` (string, optional): agent name. The mail leaves from the agent subaddress.
- `to` (array, required): recipient addresses, each a string.
- `subject` (string, optional): subject line. Required without `artifact`.
- `text` (string, optional): message body as Markdown. Trebi renders it as the HTML part in the workspace style (`email.style`). Give `text` or `html`, not both.
- `html` (string, optional): your own HTML body. Trebi makes the plain-text part from it. Use inline styles; do not link scripts or stylesheets. Write Markdown in `text` unless you need a layout that Markdown cannot give.
- `in_reply_to` (string, optional): message id this mail answers, so it lands on the thread.
- `artifact` (string, optional): an artifact id, name, or `name@n` to send. Trebi makes the mail from that version: an email draft becomes the body, a report becomes the body with a link to the full version in Trebi, and another kind becomes an attachment. The default subject is the title of the artifact. The mail records the version that it sent.
- `attachments` (array, optional): at most 10 files. Each item is an object with one of two forms. `{"path": "reports/q3.csv"}` attaches a file of the workspace; `name` is optional and sets the file name the recipient sees. `{"name": "q3.csv", "content": "<base64>"}` attaches the given bytes. `type` (string, optional) sets the media type; without it the type comes from the file name. The text, the HTML, and all files together must stay at or below 5 MiB.

### workspace_address
Read one workspace email address. The reply is masked by default: only the domain and a cut local part. The full address stays hidden unless the operator asks for it.
- `workspace` (string, required): workspace id.
- `reveal` (boolean, optional): return the full address. Ask the user before you set it.

### workspace_rotate
Mint a new address for one workspace: `<handle>-<token>@<domain>`. The old address retires; mail to it stops matching the workspace. Destructive and irreversible: open mail threads break, and replies to an earlier alert no longer arrive. Rotate only when the address leaked or the operator wants a new handle, and only when the operator asks. Run data does not change.
- `workspace` (string, required): workspace id.
- `handle` (string, optional): a new workspace handle. This is the only way to change the handle. 2 to 20 characters: a-z, 0-9, and inner dashes.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

### messages
List the envelope log, newest first: both directions, one row per message. The reply carries `messages` and, when more rows exist, `next_cursor`. Bodies never live here; they stay in the Worker store.
- `direction` (string, optional): `in` or `out`. Absent lists both.
- `workspace` (string, optional): filter by workspace id.
- `thread` (string, optional): filter by thread root, so one mail thread reads as one page.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### show
Read one envelope: sender, subject, verdicts, links to the run or conversation it caused, and attachment metadata. Fetch an attachment with `attachment`.
- `id` (string, required): message id from `messages`.

### attachment
Fetch one attachment's bytes through the Worker. The fetch is explicit and logged; nothing auto-fetches. The reply carries the bytes as `content_base64` with their `size`.
- `message` (string, required): message id that lists the attachment.
- `attachment` (string, required): attachment id from that message.
- `out` (string, optional): output path on the machine where `trebi mcp serve` runs. The bridge strips the field, writes the bytes to the file, and answers with the path, so the content stays out of the transcript. The daemon never reads this path.

### poll
Pull the Worker spool now, outside the poll tick. The reply carries `pulled`, the count of envelopes this pull routed. Use it after you expect mail and do not want to wait for the tick.

### held
List the replies that wait for the approval of the user, newest first. A reply waits when its sender is new in a thread of an agent, or when the safety check found a risk or did not finish. The agent does not read a held reply. The reply carries `items`; each item is an envelope with `decision_id` when a check ran. Read the check with the `decision` tool.
- `workspace` (string, optional): workspace id or name. Absent lists all workspaces.

### release
Give a held reply to its agent. Only a person can release a reply. When you call it for the user, the call fails with 403; ask the user to choose in the Inbox. When the safety check found a risk, the call returns `validation` with `details.confirm_required` and the reasons. Tell the user the reasons, and repeat the call with `confirm` only when the user agrees.
- `id` (string, required): message id from `held`.
- `scope` (string, optional): `once` (the default) gives only this mail. `thread` also trusts the sender in this thread. `sender` also adds the address to `email.accept` of the agent. `domain` also adds `@<domain>` to `email.accept`.
- `confirm` (boolean, optional): the user agrees to release a reply that the check marked as a risk.

### ignore
Drop a held reply. The agent never reads it. Only a person can ignore a reply.
- `id` (string, required): message id from `held`.

## Related guides

`observability`, `email-format`
