# trebi-relay/1

This document is the published contract between a paired phone (the Trebi app, `trebi-ui/mobile/relay`) and `trebid` (`internal/relay`). The cloud carries these bytes as opaque `relay_frame.bytes` (see `internal/cloudlink/testdata/contract/CONTRACT.md`, "Relay"). The fixtures in this directory define the exact bytes. `vectors_test.go` writes `handshake.v1.json`, `records.v1.json`, and `yamux.v1.json` with `-update` and checks them on every run. The Rust tests check the same files. A change is a coordinated release: change this repository first, then re-vendor into `trebi-ui/mobile/relay/contract/` byte-identical.

## Pairing link

`pair-link.v1.json` has the valid and the invalid cases. The form is:

```text
https://pair.trebi.ai/#v=1&a=<api host>&d=<device id>&k=<daemon key>&p=<psk>&c=<claim>&o=<offer id>&n=<machine name>
```

- The fragment is `application/x-www-form-urlencoded`. A browser never sends the fragment to a server.
- `k` is the daemon static X25519 public key. `p` is the pairing psk. Both are base64url with no padding, and both decode to 32 bytes.
- `a` is `host[:port]` of the cloud api. A release build allows only `trebi.ai` and its subdomains, with no port, over `https`. A debug build also allows `127.0.0.1`, `localhost`, and `10.0.2.2` (the Android emulator host) with any port, over `http`.
- `c` is the one-time claim. The phone trades it for a phone token at `POST /v1/clients`.
- `o` is the offer id. `n` is the machine name for the screen.
- The error codes are `not_pair_link`, `version`, `missing_field`, `bad_key`, and `host_not_allowed`.

## Handshake

The pattern is `Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s`. The phone is the initiator. The prologue is `trebi-relay/1`, one NUL byte, and the device id in ASCII.

1. Message 1 (phone to daemon) carries the phone static key and this JSON payload: `{"v":1,"client_id":"…","offer":"<offer id or empty>","name":"…","platform":"ios|android","app_version":"…"}`.
2. The daemon reads the phone static key from message 1. A known key selects the psk of that phone. An unknown key with a live `offer` selects the offer psk. Anything else is refused. `client_id` must be the cloud id of the phone token that opened the socket.
3. Message 2 (daemon to phone) carries `{"v":1,"machine":"…","daemon_version":"…","scope":"full|read","client_psk":"<base64, pairing only>"}`. `client_psk` is standard base64 of 32 bytes. The phone uses it as the psk of every later session.
4. The phone sends one control record `{"type":"ready"}` first. This proves that the phone has the psk. The daemon makes the pairing only after it reads this record. The daemon then sends its own `{"type":"ready"}`.

When the daemon refuses, it sends no message 2. It sends `relay_close{reason:"refused"}`, and the cloud closes the phone socket with `4409`. A handshake must finish in 15 seconds.

`handshake.v1.json` has fixed static keys, fixed ephemeral private keys (the 32 bytes that the key generator reads), the psk, the prologue, both payloads, both messages, and four transport messages.

## Records

Each Noise transport message holds one record. The plaintext of a record is at most 16,384 bytes, type byte included. The first plaintext byte is the record type:

| Type | Meaning |
|---|---|
| `0x00` | mux bytes |
| `0x01` | control JSON |

Control messages: `{"type":"ready"}` (once per side, first), and `{"type":"error","code":"revoked|scope_changed|daemon_stopping"}`. An error record ends the session. A side ignores a control type that it does not know. A record of another type, an empty record, or a decrypt failure ends the session. Noise nonces make a replayed or reordered message fail.

`records.v1.json` has one record of each type with its ciphertext (they continue the initiator cipher of `handshake.v1.json`), and the record sizes for a split of 1, 16,383, 16,384, and 40,000 mux bytes. The base64 of a full message is about 21,900 chars.

## Mux

The `0x00` records form one ordered byte stream. yamux (`github.com/hashicorp/yamux` framing) runs on it with the default windows (256 KiB). A side must not grow its windows past 256 KiB. The phone is the client and opens all streams. `yamux.v1.json` has the frames of one stream: open, data, accept, data, and close in both directions.

## Streams

Each stream carries HTTP/1.1, as the webview sends it. The `Host` header is `127.0.0.1:<port>` of the loopback proxy in the app. The daemon serves each stream with the same root handler as its loopback listener (the API and the embedded UI). The actor is `mobile:<phone name>`. The daemon removes `X-Trebi-Run-Token` from each request.

## Sessions

- A session ends when the phone socket closes. A new socket makes a new handshake. There is no rekey in v1.
- At most 8 sessions per phone and 32 in total.
- A phone with the scope `read` can use only `GET`, `HEAD`, and `OPTIONS`.
