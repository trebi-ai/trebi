# system

System operations: health, doctor, lifecycle, cloud link, and the bus poll.

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
Read the cloud link state. No input fields.

### cloud_link
Link this daemon to the cloud service. No input fields.

### cloud_unlink
Unlink this daemon from the cloud service. Destructive. Remote clients lose access.
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

## Related guides

`mcp-basics`, `observability`
