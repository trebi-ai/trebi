# harness

Harness operations: manage agent harness tools and their login state.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

Per-harness defaults live in `config.yaml` and are set with the `config` tool:

- `tools.<name>.model`: session model, passed as a launch flag.
- `tools.<name>.effort`: session reasoning effort; levels are per harness.
- `tools.<name>.env.<KEY>`: environment for every trebi launch of that harness.

A job may override model and effort under its own `harness:` node. Neither ever
writes the vendor's own config file, so a shared-mode interactive CLI is not
affected.

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

### models
List the models of a harness. `source` is `cli` when the harness CLI has a list command (cursor, grok, opencode, pi). `source` is `static` for claude, codex, and copilot: trebi ships their list, and a configured default that is not in it comes first. `source` is `none` with an empty list when the harness has no list, is not installed, or answered nothing; keep the configured model free-text then. `default` is the model a run uses with no pin.
- `name` (string, required): harness name.

### usage
Read the plan limits of each harness account. No input fields. The reply is `{"harnesses":[{"harness","plan","windows":[…]}]}`. Each window has `key` (`five_hour`, `daily`, `weekly`, `monthly`, or `custom`), `scope`, `minutes`, `used_percent` (0 to 100), `resets_at`, `source` (`run`, `probe`, or `session_file`), and `observed_at`. A window past its reset time shows 0 and no `resets_at`. Today only Codex reports limits.

### usage_refresh
Read the Codex limits now, then reply as `usage`. No input fields. A second call within one minute returns 429. A failed read still returns the stored values, with `refresh_error`.

### login_start
Start one login flow. The response carries a URI or a token prompt.
- `name` (string, required): harness name.
- `mode` (string, optional): auth mode, `shared` or `separate`. It sets where the credential lives, not the login channel.

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
