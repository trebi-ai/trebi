---
topic: email-format
title: Email format
description: Write Markdown that Trebi mails as styled HTML, with alerts, number tiles, status lists, buttons, and the workspace style.
---

# Email format

Write each mail as Markdown. Do not write HTML. Trebi sends two parts: your Markdown as the text part, and the same Markdown as an HTML part in the workspace style. A report, a reply, and `trebi email send --text` all use this path. The reader sees the HTML part. A mail client that cannot show HTML shows the Markdown, so keep the Markdown easy to read as text.

## Markdown that works

- Headings, paragraphs, **bold**, *italic*, links, lists, and tables (GitHub syntax, column alignment included).
- Task lists: `- [x] done` and `- [ ] open` show as ☑ and ☐.
- Code: `inline` and fenced blocks.
- Alerts: a block quote that starts with `[!NOTE]`, `[!TIP]`, `[!IMPORTANT]`, `[!WARNING]`, or `[!CAUTION]` shows as a colored callout.

```markdown
> [!WARNING]
> Rotate the sync token before Friday.
```

Trebi removes raw HTML from the Markdown. Images need an `https` URL. Keep a mail short. Gmail hides the end of a message that is larger than about 100 KB. For a longer mail, Trebi shows the first blocks that fit, adds a note, and attaches the whole mail as `full-mail.html`. A mail that already has 10 files goes uncut. The text part always has all of your Markdown.

## Report blocks

Three fenced blocks show as report parts. Each line is `label: field, field`. A comma followed by a space separates the fields, so `1,234` stays one value. A line that does not fit drops. A block with no valid line stays a code block.

`kpi` shows number tiles, three on each row. The fields are value, change, and tone. A change that starts with `+` shows green and one that starts with `-` shows red. The tone `good`, `bad`, or `neutral` replaces the color from the sign, for example for a cost that went down.

````markdown
```kpi
Runs: 1,234, +12%
Cost: $3.20, -4%, good
Failed: 2, +1, bad
```
````

`status` shows a list with colored dots. The fields are state and note. `ok`, `pass`, `done`, and `up` show green. `warn`, `slow`, `pending`, and `skipped` show yellow. `fail`, `failed`, `error`, and `down` show red. Other states show gray.

````markdown
```status
digest: ok
sync: failed, token expired
backup: slow, 14 min
```
````

`button` shows one row of buttons. The field is an `https`, `http`, or `mailto` URL. Other URLs drop.

````markdown
```button
Open the runs: https://example.com/runs
```
````

## Layout and style

The purpose of the mail selects the layout. A report or a send gets the report layout: the sender name or logo at the top, an accent bar, and the footer. A reply gets the reply layout, which has no header. An alert or a test gets the minimal layout, where each line break stays.

`email.style` changes the house style. Put it in the `assistant:` block of `trebi.yaml` for the workspace, and in `AGENT.md` for one agent. An agent value replaces only the fields it sets.

```yaml
email:
  style:
    accent: "#0f766e"                  # links, buttons, report header, and accent bar
    logo: https://acme.com/logo.png    # https image at the top of a report
    footer: Acme Ops · weekly          # one line below each mail
```

`accent` is a color such as `#0f766e`. `footer` is one line of at most 200 characters. A bad value fails the save.

## Your own HTML

`trebi email send --html` or `--html-file` sends your own HTML instead of Markdown. Use it only for a layout that Markdown cannot give. Give `--text` or the HTML, never both. Trebi makes the plain-text part from the HTML, so do not write a separate text version. Use inline styles, and do not link scripts or stylesheets.

## Related guides

`agents`
