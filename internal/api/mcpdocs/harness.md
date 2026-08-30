# harness

Harness operations: manage agent harness tools and their login state.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List harnesses. No input fields.

### patch
Change harness settings.
- `name` (string, required): harness name.
- `auth_mode` (string, optional): authentication mode.

### ensure
Make one harness ready for use.
- `name` (string, required): harness name.

### auth
Read the authentication state of one harness.
- `name` (string, required): harness name.
- `live` (boolean, optional): probe the live credential.

### login_start
Start one login flow. The response carries a URI or a token prompt.
- `name` (string, required): harness name.
- `mode` (string, optional): login mode, `uri` or `token`.

### login_token
Submit one API token for login.
- `name` (string, required): harness name.
- `token` (string, required): token text.

### login_code
Submit one OAuth code for login.
- `name` (string, required): harness name.
- `code` (string, required): code text.

### login_status
Read the state of the current login flow.
- `name` (string, required): harness name.

### login_cancel
Cancel the current login flow. Destructive.
- `name` (string, required): harness name.

### logout
Drop the stored credential of one harness. Destructive.
- `name` (string, required): harness name.
- `mode` (string, optional): logout mode.
- `confirm` (boolean, optional): confirm the logout.

## Related guides

`skills-secrets-config`
