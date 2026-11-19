---
topic: getting-started
title: Fresh install to first run
description: From a registered workspace to a job that runs once.
---

# Fresh install to first run

## Is the daemon up?

```bash
trebi service status --json
trebi doctor --json
```

`service status` / `service start` / `service install` are **local only**; remote: none (if you can reach MCP, the daemon is up). Health checks remotely are `system` actions `health` and `doctor`.

If the daemon was never installed: `trebi service install` then `trebi service start` (**local only**; remote: none). Never run `trebi service daemon` yourself.

## Register a workspace

```bash
trebi workspace add --name reports --create
trebi workspace use reports
```

`workspace use` is **local only**; remote: pass `--workspace reports` (MCP: `workspace` in `input`) on every workspace-scoped call.

Pathless `--create --name` mkdirs `<workspaces.root>/<name>` (default `~/trebi`). Register an existing folder with an absolute path:

```bash
trebi workspace add /path/to/repo --name reports
```

```bash
trebi workspace list --json
trebi workspace current --json
```

`workspace current` is **local only**; remote: there is no sticky context — pass `workspace` explicitly.

Move a registered folder (rewrites absolute paths in jobs and scripts; jobs should keep using absolute paths):

```bash
trebi workspace move reports /new/path/reports --force
trebi workspace move reports /new/path/reports --dry-run
```

`--force` is required for the real move. `--mkdir` creates the destination parent. If you already `mv`'d the folder, pass the new path and trebi rebinds the id.

## Create and launch a job

Each job belongs to an agent. Make an agent first when the workspace has none (`trebi guide agents`). Write a `JOB.md` (see `trebi guide jobs-and-scheduling`), then:

```bash
trebi job create daily-report --agent ops --file ./JOB.md --workspace reports
trebi job validate ops/daily-report --workspace reports --json
trebi job run ops/daily-report --workspace reports --test --json
trebi job run ops/daily-report --workspace reports --json
```

`--file` reads a local path into the job body (**local only** convenience); remote: pass the document as `content`. `--test` evaluates gates and prints the plan without launching.

```bash
trebi job list --workspace reports --json
trebi job show ops/daily-report --workspace reports --json
```

## Watch the first run

```bash
trebi runs list --workspace reports --job ops/daily-report --limit 1 --json
trebi runs show <id> --json
```

Do not `runs attach` from an agent session (**local only**; remote: poll `runs show` / `runs transcript` / `runs logs`). See `trebi guide runs-and-debugging`.
