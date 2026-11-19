---
topic: artifacts
title: Artifacts
description: Save reports and email drafts as versioned artifacts, send them, and ask for changes.
---

# Artifacts

An artifact is one named document in a workspace: a weekly report, a table, a chart, or an email draft. Each save makes a new numbered version. A version never changes, so a link in an old mail always shows what that mail said. The user reads artifacts in the Artifacts screen of the UI.

## Save from a run

A run saves a file of its working folder:

```bash
trebi artifact put weekly-report --from report.md --title "Weekly report" --note "Week 41"
```

- The name is lowercase letters, digits, and dashes. Each job, agent, and the assistant has its own names in a workspace.
- The kind comes from the file name: `.md` is `markdown`, `.html` is `html`, `.csv` is `table`, `.json` is `json`, `.svg` is `svg`, an image is `image`, `.pdf` is `pdf`. Set `--kind` to change it. `--kind email` makes a draft.
- The artifact belongs to the job or agent of the run. A later run of the same job or agent can save a new version. Another job cannot.
- Without `--from`, the command reads stdin.

A job can also name its files in `JOB.md`. At the end of a complete run, Trebi saves each file that the run did not save itself:

```yaml
artifacts:
  - path: report.html          # relative to the working folder, required
    name: weekly-sales         # default: the file name without the extension, as a slug
    title: Weekly sales report # default: the name
```

A file that is missing at the end of the run sets the outcome note of the run and opens an inbox item. The run status does not change.

## Change an artifact

Read the latest version before you change it. Then save with `--base`:

```bash
trebi artifact get weekly-report > report.md
# edit report.md
trebi artifact put weekly-report --from report.md --base 3 --note "Fix the totals"
```

When a newer version exists, the save fails: "Version N is newer. Read it with `trebi artifact get <name>`, then save with `--base N`." Merge your change into version N and save again.

## Read

```bash
trebi artifact ls                      # newest first; --owner, --kind, --q
trebi artifact show weekly-report      # the artifact and its versions
trebi artifact get weekly-report@2     # the content of version 2
trebi artifact ls --run <run-id>       # the versions that one run saved
```

## Email drafts

An `email` artifact is a mail that waits for the user. Its content starts with a header block with `kind: email`:

```markdown
---
kind: email
from: gmail-work
to: [ops@example.com]
cc: [lead@example.com]
subject: Weekly sales
in_reply_to: <msg-id@example.com>
---
Hello team,

The numbers for week 41 are below.
```

- `from` is `trebi` (the email channel of the workspace) or the slug of a mailbox connection, for example `gmail-work`.
- `trebi artifact send <name>` sends the latest version. `--draft` saves it to the drafts of the mailbox instead.
- A mailbox connection sends through its `send` operation. The user sets each operation to `on`, `ask`, or `off`. In state `ask`, the send of an agent opens a request and fails with `approval_required`. End the turn. The run continues when the user approves; then send again.
- The email channel sends to admins, to addresses on the workspace allowlist, and, from a job, to the allowlist of its own routes on the email connection and to what its owner agent accepts.

## Send a report

```bash
trebi email send --workspace ws1 --to ops@example.com --artifact weekly-report
```

Trebi makes the mail from the version: a Markdown report becomes the body with a link "Open the full report", a short HTML report becomes the body, a long one gets a summary and the full file as an attachment, and another kind becomes an attachment. The default subject is the title. `trebi artifact deliveries <name>` lists the mails that sent the artifact. Trebi never deletes a version that a mail sent.

## Talk about an artifact

Each artifact has one chat with its owner. The user opens it from the artifact page with "Chat with <owner>", or with:

```bash
trebi artifact chat weekly-report        # prints the conversation id, "new" or "existing"
trebi artifact ask weekly-report "Add the totals per region"
```

The chat goes to the conversation that saved the newest version. When there is none, Trebi makes a conversation with the agent that owns the artifact, or with the workspace assistant for a job or assistant artifact. The first chat or ask stores the chat on the artifact (`conversation_id`), and later calls reuse it. `ask` sends the text to the same chat as a message of the user, so the reply stays in the chat and does not go out by email. `trebi artifact ls --conversation <id>` lists the artifacts of a chat. A run cannot chat or ask.

The owner reads the version and saves a change from the chat:

```bash
trebi artifact get weekly-report@3
trebi artifact put weekly-report --from report.md --base 3
```

## Versions and pins

Trebi keeps the newest 30 versions of each artifact. It never deletes a pinned version, the live version, or a version that a mail sent. `trebi artifact pin <name>@<n>` keeps a version; `--off` removes the pin. Only the user can pin.

## Live artifacts

A live artifact is a Markdown or HTML template plus data sources. A source is a read-only operation of a connection with fixed input. A refresh runs the sources with no AI model and stores the result as a snapshot. The user opens the artifact and sees the newest data.

Write the sources in a YAML file:

```yaml
params:
  - name: period
    label: Period
    type: enum              # enum | date | number
    options: [7d, 30d]
    values: {7d: "7", 30d: "30"}
    default: 7d
sources:
  - name: sales             # lowercase letters, digits, and _
    connection: warehouse
    operation: query
    input:
      sql: select day, total from daily_sales order by day
      days: "{param.period}" # a parameter fills a whole value only
```

Then save the template with the sources:

```bash
trebi artifact put sales-dashboard --from dashboard.md --sources sources.yaml
trebi artifact refresh sales-dashboard --input period=30d
trebi artifact get sales-dashboard            # the template with the newest data
trebi artifact get sales-dashboard --raw      # the template only
trebi artifact snapshots sales-dashboard      # the snapshots, newest first
```

- A Markdown template uses Go template syntax. `.sales.rows` is the list of records, `.sales.columns` the column names, `.sales.error` the error text, and `.params.period` the value of a parameter.
- The functions are `table`, `kpi`, `num`, `money`, `pct`, `delta`, `date`, `first`, `last`, and `sum`. For example: `{{ table .sales 20 }}` and `{{ kpi "Revenue" (money (sum .sales.rows "total")) "" }}`.
- An HTML template reads `window.trebi.data` and `window.trebi.inputs`. The page has no network access.
- A new version with no `--sources` keeps the sources of the latest version.

When a run adds or changes a source, the user must allow it first. The save prints a line about it, and the user gets an inbox item. Until the user allows it, a refresh uses the last allowed version. The user decides with `trebi artifact approve <name>@<n>` or `trebi artifact deny <name>@<n>`.

An agent can refresh one artifact once a minute. A sooner refresh fails with `rate_limited`.

The user can refresh an artifact on a schedule and send each snapshot by email:

```bash
trebi artifact schedule sales-dashboard --cron "0 9 * * 1" --timezone Europe/Berlin --email ops@example.com
trebi artifact schedule sales-dashboard --every 24h
trebi artifact schedule sales-dashboard --off
```

Only the user can approve, deny, or schedule.

## Connection operations

An operation is a named command of a connection, for example `query` on a warehouse. Trebi runs it with the secrets of the connection, so a run never sees them:

```bash
trebi connector operations warehouse
trebi connector call warehouse query --input sql="select region, sum(total) from sales group by 1"
trebi connector operation warehouse query ask   # user only: on | ask | off
```

A run can call only the connections of its job or agent. CSV output prints as a table, JSON prints as JSON.
