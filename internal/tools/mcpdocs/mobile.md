# mobile

Phone operations: pair a phone or a tablet with this machine, list the paired phones, rename one, change its access, and remove one. A paired phone uses the Trebi app over Trebi Cloud. The traffic is encrypted end to end, so the cloud cannot read it.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

Pairing needs a machine that is linked to Trebi Cloud with the Pro Cloud plan. Without a link the error is `not_linked`. Without the plan the error is `remote_access_required`. When the user turned off phone access (config `cloud.mobile`), the error is `mobile_off`.

## Actions

### pair
Open a pairing and get its link. The reply is `{"id", "name", "link", "expires_at", "uses"}`. A chat cannot show a QR code, so show the link to the user. Tell the user to open the link on the phone, or to scan the QR code in Settings > Phones and tablets. The link is a secret: show it once and do not save it. The pairing ends when it expires, when it is used `uses` times, or when the daemon restarts.
- `name` (string, optional): a name for the phone, for example `Studio iPad`.
- `ttl` (string, optional): how long the link works, from `1m` to `720h`. The default is `10m`.
- `uses` (number, optional): how many phones can use the link, from 1 to 10. The default is 1.

### cancel_pairing
Close an open pairing before it expires.
- `id` (string, required): the pairing id from `pair`.

### list
List the paired phones. The reply is `{"items": [{"id", "name", "platform", "scope", "created_at", "last_seen_at", "connected"}]}`. `scope` is `full` or `read`.

### update
Rename a phone or change its access. A change of `scope` ends the open sessions of the phone, and the app connects again with the new access.
- `id` (string, required): the phone id.
- `name` (string, optional): the new name.
- `scope` (string, optional): `full` (all changes) or `read` (view only).

### remove
Remove a phone. It loses access to this machine at once and must pair again. Ask the user first, then repeat the call with `confirm: true`.
- `id` (string, required): the phone id.
- `confirm` (boolean, required): true once the user agrees.

## Related guides

`mcp-basics`
