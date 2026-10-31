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
List harnesses. No input fields. Each row has `name`, `label`, `vendor`, `home`, `bin_path`, `enabled` (the user turned it on), `installed` (a binary in the bin store or on PATH), `terminal_login` (a vendor login in the user home), `installable`, `hidden` (Advanced mode only), `auth_mode`, `login_modes`, `keychain_bound`, `auth_ok`, `auth_status`, `auth_detail`, `auth_account` (the email or the provider list, for display), `auth_probed_at`, `auth_expires_at`, `model`, `effort`, `effort_levels` (a list; `null` for a free variant name; empty when the harness has no effort), `approval` (`ask` or `auto`), and `asks_approval` (false when the harness cannot ask before it uses a tool; such a harness runs only with `approval: auto`). The version fields: `version` (the version that runs use), `latest_version`, `update_available`, `updates` (`auto` or `keep`), `can_install` (Trebi can install the program), `can_update` (Trebi can update this harness), `can_choose_version` (`version` can install a named version), `updating` (an update runs now or waits for the end of a run), `installs` (the versions that Trebi installed, oldest first), and `last_update` (`{at, from, to, ok, error}` or `null`). `own_files` is the number of config files in "Trebi's own" mode (see `files`).

### patch
Change the settings of one harness. Set at least one field. The reply is the full harness row, as in `list`.
- `name` (string, required): harness name.
- `enabled` (boolean, optional): turn the harness on or off. A job on a harness that is off does not run. The default harness for new jobs cannot be turned off.
- `auth_mode` (string, optional): `shared` (the same login as the terminal) or `separate` (a login for Trebi only).
- `model` (string, optional): the default model of new runs. `""` clears it.
- `effort` (string, optional): the default effort level, from `effort_levels`. `""` clears it.
- `approval` (string, optional): `ask` (Trebi asks the user before a job uses a tool) or `auto` (jobs use tools with no approval). Grok cannot ask, so a Grok job runs only with `auto`.
- `updates` (string, optional): `auto` (Trebi updates the program) or `keep` (Trebi runs a kept copy).
- `version` (string, optional): the version to keep or to install.
- `compat` (object, optional): which Claude files Grok and OpenCode also read: `{"claude": {"agents", "skills", "rules", "mcps", "hooks"}}`, each a boolean.

### ensure
Make one harness ready for use.
- `name` (string, required): harness name.

### install
Install the harness program on this machine. Trebi installs its own copy when the harness has a package; else it runs the vendor install. The user's own copy of the program does not change. The reply is the full harness row. A harness that Trebi cannot install returns 422.
- `name` (string, required): harness name.
- `version` (string, optional): the version to install. Empty installs the newest version.

### update
Update the harness program now. Trebi keeps the old program, installs the new one, and checks it. A failed check goes back to the old program; read `last_update` in the reply. The reply is the full harness row. When a run uses the harness, the update starts when the run ends and the reply is 202.
- `name` (string, required): harness name.

### check_versions
Look for a new release of each harness now. The reply is the full harness row.
- `name` (string, required): harness name.

### auth
Read the authentication state of one harness.
- `name` (string, required): harness name.
- `live` (boolean, optional): probe the live credential.

### models
List the models of a harness. `source` is `cli` when the harness CLI has a list command (cursor, grok, opencode, pi). `source` is `static` for claude, codex, and copilot: trebi ships their list, and a configured default that is not in it comes first. `source` is `none` with an empty list when the harness has no list, is not installed, or answered nothing; keep the configured model free-text then. `default` is the model a run uses with no pin. Each model has `id`, `label`, `default`, and for OpenCode also `group` (the provider), `free` (no cost), and `variants` (the variant names, in order). `efforts` lists the effort levels (`null` for a free variant name, empty for none), and `effort_default` is the effort a run uses with no pin, when known.
- `name` (string, required): harness name.

### usage
Read the plan limits of each harness account. No input fields. The reply is `{"harnesses":[{"harness","plan","windows":[…]}]}`. Each window has `key` (`five_hour`, `daily`, `weekly`, `monthly`, or `custom`), `scope`, `minutes`, `used_percent` (0 to 100), `resets_at`, `source` (`run`, `probe`, or `session_file`), and `observed_at`. A window past its reset time shows 0 and no `resets_at`. Today only Codex reports limits.

### usage_refresh
Read the Codex limits now, then reply as `usage`. No input fields. A second call within one minute returns 429. A failed read still returns the stored values, with `refresh_error`.

### login_start
Start one login flow. Read the progress with `login_status`; the bus topic `harness.login.updated` carries the same phase, URL, and code.
- `name` (string, required): harness name.
- `mode` (string, optional): auth mode, `shared` or `separate`. It sets where the credential lives, not the login channel.
- `provider` (string, optional): OpenCode only. The provider to log in to, from `providers`. Only providers with the `browser` method can start a login flow.

### login_token
Submit one API key for login.
- `name` (string, required): harness name.
- `token` (string, required): the key.
- `provider` (string, optional): OpenCode only, and required there. The provider of the key, from `providers`.

### providers
List the OpenCode model providers. OpenCode only. The reply is `{"providers":[{"id","label","group","note","logged_in","methods","key_url"}]}`. `group` is `opencode` (OpenCode Go and Zen) or `other`. `methods` holds `browser` and `key`.
- `name` (string, required): `opencode` or `opencode2`.

### files
List the vendor config files of a harness. The reply is `{"files":[…]}`. Each row has `key`, `label`, `format` (`json`, `toml`, or `markdown`), `mode` (`yours`: runs use the user file; `own`: runs use Trebi's copy), `inherit` (how `yours` works: `linked`, `copied` without the hooks, or `read` by the CLI itself), `user_path`, `trebi_path`, `user_exists`, `user_modified_at`, `own_created_at`, `user_changed_since_own`, and `backup_until` (a reset copy can be restored until then). Trebi never writes the user file.
- `name` (string, required): harness name.

### file_read
Read one config file. The reply is `{"content","etag"}`.
- `name` (string, required): harness name.
- `key` (string, required): the file key from `files`.
- `side` (string, optional): `user` or `trebi`. Empty reads the file that runs use.

### file_own
Make Trebi's own copy of a file, so that runs use it and not the user file.
- `name` (string, required): harness name.
- `key` (string, required): the file key from `files`.
- `start` (string, optional): `user` (copy the user file; the default), `empty`, or `backup` (restore the last reset copy).

### file_write
Replace Trebi's copy of a file. The file must be in `own` mode (409 otherwise). A stale `etag` returns 412 with the current `content` and `etag` in the details. A JSON or TOML error returns 422 with `line` and `column`. The limit is 1 MiB.
- `name` (string, required): harness name.
- `key` (string, required): the file key from `files`.
- `content` (string, optional): the full new text. Empty clears the file.
- `etag` (string, optional): the etag from `file_read`.

### file_reset
Go back to the user file. Trebi moves its copy to a backup for 30 days. Destructive.
- `name` (string, required): harness name.
- `key` (string, required): the file key from `files`.
- `confirm` (boolean, optional): confirm the reset.

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

### hooks
Read if Trebi sees the sessions of one harness that the user starts outside Trebi. The reply is `{"harness", "state"}`. `state` is `on`, `off`, `outdated` (turn it on again to update), `untrusted` (the harness asks the user to trust the hooks), or `unsupported` (the harness cannot report its sessions).
- `name` (string, required): harness name.

### set_hooks
Turn on or off the hooks that show the sessions of one harness that start outside Trebi. Trebi writes the hooks into the user login of the harness. The reply is as in `hooks`.
- `name` (string, required): harness name.
- `on` (boolean, required): `true` to show the sessions, `false` to stop.

## Related guides

`skills-secrets-config`
