# setup

Setup operations: read and change the first-run setup of Trebi. The setup has four steps: `you`, `ai`, `first_job`, and `done`. The state also holds the Home cards that the user hid, the first job, and the default workspace. Use this tool when the user asks to set up Trebi, to hide a Home card, or to run the setup again.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

The state has `step`, `completed_at` (null until setup is done), `hidden` (a list of `first_run`, `set_up`, and `pro`), `first_job_id`, `workspace_id`, and `revision`. Each change adds 1 to `revision` and sends the `onboarding.updated` bus topic.

The name, the time zone, and the interests of the user are config keys: `user.name`, `user.timezone`, and `user.interests` (see the `config` tool). A job with no time zone uses `user.timezone`.

## Actions

### status
Read the setup state. A first run answers `step: you` and `revision: 0`.

### update
Change the setup state. A field that you omit stays.
- `step` (string, optional): `you`, `ai`, `first_job`, or `done`.
- `completed` (boolean, optional): `true` ends the setup and sets `step` to `done`, unless you give `step`. `false` clears `completed_at`.
- `hide` (list, optional): Home cards to hide: `first_run`, `set_up`, `pro`.
- `unhide` (list, optional): Home cards to show again.
- `first_job_id` (string, optional): the id of the first job that the user made in setup.
- `revision` (number, optional): the revision that you read. A different stored revision returns `conflict`.

An unknown step or card returns `validation`.

### defaults
Make the defaults that need no screen. The call makes the `Personal` workspace in the workspaces folder, and it turns on the AI app when exactly one has a working login. A second call makes nothing new. The reply is `{"workspace", "harness", "choose"}`. `choose` is `true` when no AI app or more than one has a login, so the user must choose.

### restart
Go back to the first step. The call keeps the hidden cards, the default workspace, and the first job.

## Related guides

`setup`, `first-job`
