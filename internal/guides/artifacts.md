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
- The email channel sends only to admins and to addresses on the allowlist of the workspace.

## Send a report

```bash
trebi email send --workspace ws1 --to ops@example.com --artifact weekly-report
```

Trebi makes the mail from the version: a Markdown report becomes the body with a link "Open the full report", a short HTML report becomes the body, a long one gets a summary and the full file as an attachment, and another kind becomes an attachment. The default subject is the title. `trebi artifact deliveries <name>` lists the mails that sent the artifact. Trebi never deletes a version that a mail sent.

## Ask for a change

The user can ask the maker of an artifact for a change from the UI or with:

```bash
trebi artifact ask weekly-report "Add the totals per region"
```

The text goes to the conversation, the run, or the assistant that made the version. A run cannot ask.

## Versions and pins

Trebi keeps the newest 30 versions of each artifact. It never deletes a pinned version, the live version, or a version that a mail sent. `trebi artifact pin <name>@<n>` keeps a version; `--off` removes the pin. Only the user can pin.

## Connection operations

An operation is a named command of a connection, for example `query` on a warehouse. Trebi runs it with the secrets of the connection, so a run never sees them:

```bash
trebi connector operations warehouse
trebi connector call warehouse query --input sql="select region, sum(total) from sales group by 1"
trebi connector operation warehouse query ask   # user only: on | ask | off
```

A run can call only the connections of its job or agent. CSV output prints as a table, JSON prints as JSON.
