# system

System operations: health, doctor, lifecycle, cloud link, license, and the bus poll.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### health
Read service health. No input fields.

### overview
Read the one-call operator view: version, doctor counts, harness auth, workspace
count, running and waiting runs, unread inbox, and the next due schedule. Call it
first to answer "is trebi ready?" or "what needs me?". No input fields.

### doctor
Run the doctor checks and list findings. No input fields.

### doctor_fix
Apply one doctor fix.
- `finding_id` (string, required): finding id from `doctor`.

### logs
Read the ops log tail. No input fields.
- `max_bytes` (number, optional): cap the body at this size; default 64 KB. A cut body returns `{"content": "…", "truncated": true}`.

### version
Check for a newer trebi release. No input fields.

### drain
Stop automatic producers and reject new automatic launches with 503 `draining`. Manual launches still pass. Not destructive.

### undrain
Restore automatic producers. No input fields.

### shutdown
Stop the daemon. Destructive. In-flight runs finish first; scheduled jobs stop
firing until it starts again.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

### cloud_status
Read the cloud link state: link state, account, device, plan, granted features, and socket state. An unlinked daemon answers `linked: false`; that is the answer, not an error. No input fields.

### cloud_addresses
List the hosted mail addresses of this device, one per workspace. The addresses are masked by default.
- `reveal` (boolean, optional): return the full addresses and write an audit entry.

### cloud_link
Link this daemon to a Trebi cloud account and show the plan and features on success. The account page mints the code under "Link a device"; it expires in 10 minutes. A daemon that is already linked answers `conflict`: run `cloud_unlink` first.
- `enrollment_token` (string, required): the code from the account page. Hyphens and case are ignored.
- `name` (string, optional): device name shown on the account page. Defaults to the hostname.

### cloud_unlink
Unlink this daemon from the cloud service. Destructive. Remote clients lose access. This does not revoke the device in the cloud; the account page revokes.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

### hook_create
Mint one hosted webhook and return its public URL once, in full. Post events to that URL with any subpath: the first path segment becomes the event label. Handle the URL like a secret: anyone holding it can post an event.
- `workspace` (string, required): workspace id or name the hook belongs to.
- `label` (string, required): short name for the hook, shown in listings.

### hook_list
List the hosted webhooks. URLs are masked by default because the token is the URL.
- `reveal` (boolean, optional): return full URLs and write an audit entry.

### hook_delete
Revoke one hosted webhook. Destructive. Deliveries to its URL stop and its jobs stop firing.
- `id` (string, required): hook id from `hook_list`.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

### bus_poll
Read new events from the bus, then stop. There is no follow mode: pass `next_cursor` back on the next call. The response is `{"events": […], "next_cursor": N}`.
- `topics` (string, optional): comma-separated topic filter.
- `cursor` (string or number, optional): last seen sequence number. Pass it as a string when it is large; a JSON number above 2^53 loses precision. Omit it to read from the start of the retained log.

### guides
List workflow guide topics. No input fields. Response is `{"items":[{"topic","title","description"}]}`.

### guide
Read one workflow guide body.
- `topic` (string, required): guide topic from `guides`.

### license_show
Read the license state: supported flag, source, plan, subject, expiry, and the feature list. Works with no license installed. No input fields.

### license_install
Install a license key. Ask the user for the key first; never invent one. A build without license support answers 400 `validation`.
- `license` (string, required): the license key the operator gave you.

## Related guides

`mcp-basics`, `observability`
