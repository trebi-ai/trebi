# Oracle: connector-channel-reply

```bash
trebi connector show chat --json
trebi connector test chat --event '{"id":"eval-1","type":"message","room":{"id":"eval-user","kind":"dm"},"sender":{"id":"eval-user"},"text":"ping"}'
for _ in $(seq 1 90); do
  grep -q '"method":"messages/send"' "$EVAL_WORKDIR/replies.txt" 2>/dev/null && break
  sleep 2
done
trebi conversation list --json
grep '"method":"messages/send"' "$EVAL_WORKDIR/replies.txt"
```

The seed runs the chat connector as a trebi-connector/1 sandbox adapter (`fake connector`). The adapter records each request it receives in `replies.txt`. The seed also gives the ops assistant a trigger on the connector. The test event runs through the real pipeline, so the assistant takes one turn on the conversation of room `eval-user`.

Pass when the conversation holds an outbox row with `status: sent` for the `chat` instance and `replies.txt` holds a `messages/send` request. A reply that stays `pending` or `failed` is not a reply.
