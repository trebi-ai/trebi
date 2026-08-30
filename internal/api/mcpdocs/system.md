# system

System operations: health, doctor, lifecycle, cloud link, and the bus poll.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### health
Read service health. No input fields.

### doctor
Run the doctor checks and list findings. No input fields.

### doctor_fix
Apply one doctor fix.
- `finding_id` (string, required): finding id from `doctor`.

### logs
Read the ops log tail. No input fields.

### update
Check for a newer trebi release. No input fields.

### drain
Stop automatic producers and reject new automatic launches with 503 `draining`. Manual launches still pass. Not destructive.

### undrain
Restore automatic producers. No input fields.

### shutdown
Stop the daemon. Destructive. In-flight runs finish first.
No input fields.

### cloud_status
Read the cloud link state. No input fields.

### cloud_link
Link this daemon to the cloud service. No input fields.

### cloud_unlink
Unlink this daemon from the cloud service. Destructive.
No input fields.

### bus_poll
Read new events from the bus, then stop. There is no follow mode: pass `next_cursor` back on the next call. The response is `{"events": […], "next_cursor": N}`.
- `topics` (string, optional): comma-separated topic filter.
- `cursor` (number, optional): last seen sequence number. Omit it to read from the start of the retained log.
