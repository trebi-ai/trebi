# notify

Notify operations: read the alert route of a scope, and send a test message along it.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

An alert goes along a route: an ordered list of steps `{via, to}`. `via` is `email` or a connector with a `channel` block. For email, `to` is `contact` (the default), `admins`, or one address. For a connector, `to` is the channel ref, for example a phone number. The first step that sends stops the route. A failed step moves the alert to the next step. When no step is left, the alert stays in the inbox.

The route comes from the first layer that sets `notify`, in this order: the job, the owner agent, the workspace (`trebi.yaml`), and the global config (`notify.route`). With no layer set, the route is `[{via: email, to: contact}]`. `notify: []` sends nothing. The contact follows the same order; the global contact is `email.admins`.

## Actions

### route
Read the resolved route. The reply is `{"steps": [{"via","to","targets","problem"}], "source", "contact", "contact_source"}`. A step with a `problem` is skipped at send time: `email_off`, `no_contact`, `unknown_connector`, or `no_channel`.
- `workspace` (string, optional): workspace name or id.
- `agent` (string, optional): agent name, for example `agents/cmo`.
- `job` (string, optional): job name. Needs `workspace`.

### test
Send one test message per usable step, or only for `step`. It tests every step and does not stop at a failure. Each step gets one attempt now. The reply is `{"route": {...}, "results": [{"step","outbox_id","status","error"}]}`. The rows show in the outbox with `purpose: test`.
- `workspace` (string, optional): workspace name or id.
- `agent` (string, optional): agent name.
- `job` (string, optional): job name.
- `step` (number, optional): the step index to test.

## Related guides

`inbox-and-alerts`
