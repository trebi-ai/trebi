---
topic: first-job
title: Make the first job in setup
description: How the assistant makes the first job of a new user and records it in the setup state.
---

# Make the first job in setup

Use this guide in the `first_job` step of setup. The user says in plain words what Trebi must do for them. Make one job from that.

1. Read `trebi setup status`. Use `workspace_id` as the workspace. When it is empty, call `trebi setup defaults` first.
2. Write the job in the `Personal` workspace with `disabled: true`. The user turns it on after they read it. See `trebi guide jobs-and-scheduling` for the file shape.
3. Do not set a time zone in a cron trigger. The job then uses `user.timezone`.
4. Run the job once with `dry_run`. Fix every error before you go on.
5. Record the job in the setup state:

```bash
trebi setup update --first-job-id <job-id>
```

6. Tell the user what the job does and when it runs. Ask them to turn it on.

Use the interests in `user.interests` to pick an example when the user does not know what to ask for.
