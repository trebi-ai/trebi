# email

Email channel operations: status, test message, workspace addresses, the envelope log, and the inbound poll.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

The channel is off until the operator configures the `email` block in the daemon config. `status` works with the block absent and reports why the channel is off. A workspace address is masked unless you pass `reveal`; the full address is a secret the operator decides to show.

## Actions

### status
Read the email channel state: configured flag, worker and domain, admin list (`admins`), poll interval, DMARC policy, entitlement, address count, and the last poll result. Works with the block absent; then `configured` is false and the reply names the cause. No input fields.

### test
Send one fixed test message to the admins (`email.admins`). Use it after you change the config, to prove the path to the mailbox works. The reply carries `message_id`. No input fields.

### send
Send one explicit message from the workspace address, or from the agent subaddress when `agent` is set. The reply carries `message_id`. Recipient rule: every address in `to` must be an admin or on the workspace allowlist; anything else answers 400 `validation`. Ask the user to add the address to the allowlist before you send to a third party.
- `workspace` (string, required): workspace id or name.
- `agent` (string, optional): agent name. The mail leaves from the agent subaddress.
- `to` (array, required): recipient addresses, each a string.
- `subject` (string, required): subject line.
- `text` (string, required): message body.
- `in_reply_to` (string, optional): message id this mail answers, so it lands on the thread.

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

## Related guides

`observability`
