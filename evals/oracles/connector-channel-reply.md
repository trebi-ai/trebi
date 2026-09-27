# Oracle: connector-channel-reply

```bash
trebi connector show chat
trebi connector test chat --event '{"chat_id":"eval-user","text":"ping"}'
trebi conversation list --json
trebi conversation outbox <id> --json
cat "$EVAL_WORKDIR/replies.txt"
```

The seed gives the chat connector a channel whose `send` command appends its
stdin to `replies.txt`, and an assistant trigger on the connector. The test
event runs through the real pipeline, so the assistant takes one turn on the
conversation the channel points at.

Pass when the conversation holds an outbox row with `status: sent` for the
`chat` connector and `replies.txt` holds at least one character. A reply that
stays `pending` or `failed` is not a reply.
