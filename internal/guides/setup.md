---
topic: setup
title: First-run setup
description: The setup state, the defaults that need no screen, and how to run setup again.
---

# First-run setup

The first run of Trebi has four steps: `you`, `ai`, `first_job`, and `done`. The state is one record with a `revision`. Read it before you change it.

```bash
trebi setup status
```

## What the user sets

The name, the time zone, and the interests are config keys. A job with no time zone of its own uses `user.timezone`. When the key is empty, it uses the time zone of this machine.

```bash
trebi config set user.name "Ada"
trebi config set user.timezone Europe/Berlin
trebi config set user.interests email,reports
```

The interests are `email`, `reports`, `code`, `research`, `sales`, and `watch`. A time zone that Trebi does not know gives a validation error.

## Defaults with no screen

```bash
trebi setup defaults
```

The call makes the `Personal` workspace in the workspaces folder. It turns on the AI app when exactly one has a working login. The reply has `choose: true` when no AI app or more than one has a login. Then ask the user which one to use. A second call makes nothing new.

The call does not make an email address. A free account has no email. Ask for the email name of a workspace only on a plan that has email.

## Move through the steps

```bash
trebi setup update --step ai
trebi setup update --step done --completed
trebi setup update --hide pro
```

Send the `revision` that you read. When another client changed the state, the call fails with `conflict`. Read the state again and retry.

## Run setup again

```bash
trebi setup restart
```

The call goes back to the first step. It keeps the hidden Home cards, the default workspace, and the first job.
