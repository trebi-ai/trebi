---
topic: requests
title: Requests
description: Ask a person for a decision from a job run, end the turn, and continue with the answer.
---

# Requests

A request is a decision point that a job run opens for a person. It is a batch of forms: each form is one question. The run asks, ends its turn, and waits with status `parked`. Nothing runs while it waits. When the person answers all forms, Trebi continues the same session. The first prompt of the new run holds the answers. Requests are a Pro feature. When the plan grants requests, every job and agent run gets a short version of this guide at the end of its first prompt. A job prompt then states only what to ask.

## When to ask

- Ask before an action that a person must approve: a merge, a sent mail, a public comment, a payment.
- Ask when the job cannot continue without a fact that only a person has.
- Do not ask for a thing that the job can decide alone. Every request stops the work until a person answers.

## One request for each turn

The `create` calls of one turn join one request. The first call opens the request. Each next call adds its forms while the request is pending. A request holds at most 10 forms. The person answers all forms together, so ask everything that the next step needs in the same turn. A chain limit request from Trebi is separate.

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
trebi request create --title "Ship the release" --forms-file forms.json
```

The flags make one form. `--forms-file` takes a JSON array of forms with the same fields as the MCP `create` action: `[{"type": "confirm", "title": "Tag v0.15"}, {"type": "choose", "title": "Which notes?", "options": [{"id": "a", "label": "Short"}]}]`.

After `create`, **end your turn**. Do not wait and do not poll. With the pane runtime, print the completion line. Trebi continues the session with the answers.

The answers arrive as one prompt that starts with `[trebi] The person answered request`. It has one fenced JSON block for each form, with `idx` and `decision`. The JSON is the source of truth:

- `approve`: do the action as proposed. For `review`, use your original draft.
- `edit`: use `text` exactly. Do not use your original draft.
- `reject`: do not do the action. Read `note`. Finish, or make a new request.
- `respond`: nothing is approved. Read `message` and continue.
- `choose`: continue with `choices`.
- `submit`: continue with `values`.
- `expired`: nothing is approved. The person did not answer in time.

## Answer as a person

```bash
trebi request list --status pending
trebi request show <id>
trebi request answer <id> approve --note "Ship it."
trebi request answer <id> edit --text-file reply.md
trebi request answer <id> respond --message "Wait for the release notes."
trebi request answer <id> choose --choice v2
trebi request answer <id> submit --values-file values.json
trebi request answer <id> --answers-file answers.json
trebi request dismiss <id>
```

The decision and its flags answer a request with one form. A request with more forms needs `--answers-file`: a JSON array with one answer for each form, `[{"idx": 0, "decision": "approve"}, {"idx": 1, "decision": "choose", "choices": ["a"]}]`.

A job run cannot answer a request, not even its own. The run page shows the request in its Session tab with the reports and the decision controls. An email reply of `approve` or `reject` also answers a request with one form.
