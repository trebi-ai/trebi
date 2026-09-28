---
topic: requests
title: Requests
description: Ask a person for a decision from a job run, end the turn, and continue with the answer.
---

# Requests

A request is a decision point that a job run opens for a person. The run asks, ends its turn, and waits with status `parked`. Nothing runs while it waits. When the person answers, Trebi continues the same session. The first prompt of the new run holds the answer. Requests are a Pro feature.

## When to ask

- Ask before an action that a person must approve: a merge, a sent mail, a public comment, a payment.
- Ask when the job cannot continue without a fact that only a person has.
- Do not ask for a thing that the job can decide alone. Every request stops the work until a person answers.

## Which type

| Type | Use | Payload |
|---|---|---|
| `confirm` | One action: merge PR #42, send this mail | none |
| `review` | A draft the person can change | `draft{format, text}` |
| `choose` | One or more of a list | `options[]{id, label, detail}`, `multi` |
| `form` | A few fields | `schema` (flat JSON Schema) |

## Ask from a job

```bash
trebi request create --type review --title "Reply to issue #812" \
  --draft-file out/reply.md --target "GitHub comment on trebi-ai/trebi#812" \
  --report out/triage.html --expires-in 24h
trebi request create --type confirm --title "Merge PR #42"
trebi request create --type choose --title "Which release?" --option v1=v0.14 --option v2=v0.15
trebi request create --type form --title "Ship details" --schema-file form.json
```

After `create`, **end your turn**. Do not wait and do not poll. With the pane runtime, print the completion line. Trebi continues the session with the answer.

The answer arrives as one prompt that starts with `[trebi] The person answered`. It has one fenced JSON block for each request. The JSON is the source of truth:

- `approve`: do the action as proposed. For `review`, use your original draft.
- `edit`: use `text` exactly. Do not use your original draft.
- `reject`: do not do the action. Read `note`. Finish, or make a new request.
- `respond`: nothing is approved. Read `message` and continue.
- `choose`: continue with `choices`.
- `submit`: continue with `values`.
- `expired`: nothing is approved. The person did not answer in time.

A short script can wait for the answer in the same run (`poll` delivery):

```bash
trebi request create --type confirm --title "Deploy now?" --wait 10m
# exit 0: approve/edit/choose/submit; 1: reject/respond; 2: expired/cancelled; 3: wait timeout
```

## Answer as a person

```bash
trebi request list --status pending
trebi request show <id>
trebi request answer <id> approve --note "Ship it."
trebi request answer <id> edit --text-file reply.md
trebi request answer <id> respond --message "Wait for the release notes."
trebi request answer <id> choose --choice v2
trebi request answer <id> submit --values-file values.json
trebi request dismiss <id>
```

A job run cannot answer a request, not even its own. The inbox has a Requests tab with the report and the decision controls. An email reply of `approve` or `reject` also answers.
