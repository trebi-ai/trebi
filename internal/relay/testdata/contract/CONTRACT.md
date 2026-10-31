# trebi-relay/1

This document is the published contract between a paired phone (the Trebi app, `trebi-ui/mobile/relay`) and `trebid` (`internal/relay`). The cloud carries these bytes as opaque `relay_frame.bytes` (see `internal/cloudlink/testdata/contract/CONTRACT.md`, "Relay"). The fixtures in this directory define the exact bytes. `vectors_test.go` writes `handshake.v1.json`, `records.v1.json`, `direct.v1.json`, and `yamux.v1.json` with `-update` and checks them on every run. The Rust tests check the same files. A change is a coordinated release: change this repository first, then re-vendor into `trebi-ui/mobile/relay/contract/` byte-identical.

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

### Backup address

A network can block the Cloudflare IPs of `trebi.ai` and leave the `workers.dev` address of the same Worker open. The link always names the main host. The app has a fixed map from the main host to the backup host. The map is the only source of backup hosts: the app never takes a `workers.dev` host from a link, because any person can deploy a `workers.dev` Worker.

| Main host | Backup host |
|---|---|
| `staging-api.trebi.ai` | `trebi-cloud-api-staging.ocral.workers.dev` |
| `api.trebi.ai` | `trebi-cloud-api.ocral.workers.dev` |

Each valid `release` case in `pair-link.v1.json` has `api_fallback`: the backup address, or `null` when the host has no entry. The app tries the main address first. It tries the backup address only when the connection does not open: a dial error, a DNS error, a TLS handshake error, or a connect timeout of 8 seconds. An HTTP answer never moves the app to the backup address. The signature and the Noise handshake have no host, so the same credentials work on both addresses. `internal/cloudlink/testdata/contract/CONTRACT.md`, "Addresses", has the daemon side.

## Handshake

The pattern is `Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s`. The phone is the initiator. The prologue is `trebi-relay/1`, one NUL byte, and the device id in ASCII.

1. Message 1 (phone to daemon) carries the phone static key and this JSON payload: `{"v":1,"client_id":"…","offer":"<offer id or empty>","name":"…","platform":"ios|android|machine","app_version":"…"}`.
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

Control messages: `{"type":"ready"}` (once per side, first), `{"type":"error","code":"revoked|scope_changed|daemon_stopping"}`, and `{"type":"direct","v":1,"addrs":[…]}` (see "Direct transport"). An error record ends the session. A side ignores a control type that it does not know. A record of another type, an empty record, or a decrypt failure ends the session. Noise nonces make a replayed or reordered message fail.

`records.v1.json` has one record of each type with its ciphertext (they continue the initiator cipher of `handshake.v1.json`), and the record sizes for a split of 1, 16,383, 16,384, and 40,000 mux bytes. The base64 of a full message is about 21,900 chars.

## Mux

The `0x00` records form one ordered byte stream. yamux (`github.com/hashicorp/yamux` framing) runs on it with the default windows (256 KiB). A side must not grow its windows past 256 KiB. The phone is the client and opens all streams. `yamux.v1.json` has the frames of one stream: open, data, accept, data, and close in both directions.

## Streams

Each stream carries HTTP/1.1, as the webview sends it. The `Host` header is `127.0.0.1:<port>` of the loopback proxy in the app. The daemon serves each stream with the same root handler as its loopback listener (the API and the embedded UI). The actor is `mobile:<phone name>`. The daemon removes `X-Trebi-Run-Token` from each request.

## Sessions

- A session ends when the phone socket closes. A new socket makes a new handshake. There is no rekey in v1.
- At most 8 sessions per phone and 32 in total.
- A phone with the scope `read` can use only `GET`, `HEAD`, and `OPTIONS`.

## Machines

Another Trebi machine of the same account can be the initiator in place of a phone (the peer link). It reads the same pairing link, with the same rules, and gets its client token from `POST /v1/clients/peers` (`internal/cloudlink/testdata/contract/CONTRACT.md`, "Machine clients"). Then it runs the same handshake, records, mux, and streams as a phone.

- `hello.platform` is `machine`. `hello.name` is the name of the source machine. `hello.app_version` is the Trebi version of the source machine.
- The target serves each stream with the actor `machine:<name>`. The scope is always `full`.
- The source sends a request for the person at its UI. It never sends a request of an agent, a job, or the assistant. The target removes `X-Trebi-Machine` from each request, so a request never goes on to a third machine.
- A machine uses only the relay in v1. It does not use the direct transport.

## Direct transport

A phone on the same network as the machine can skip the cloud. The direct path runs the same session as the relay: the same pattern, the same prologue (`trebi-relay/1`, NUL, device id), the same records, the same yamux settings, and the same HTTP streams. Only the transport changes.

- The transport is plain TCP. Noise gives confidentiality and identity, so there is no TLS.
- Each Noise message has a 2-byte big-endian length before it. The length is 1 to 65,535. A length of 0 or a close inside a frame ends the session. A sender never frames an empty message or a message longer than 65,535 bytes.
- The phone is the initiator. The daemon listens on `mobile.direct.port` (default 7434) on all interfaces. The `/v1` listener stays on `127.0.0.1`.
- The daemon accepts only a phone that is already paired. The Noise initiator static key must be a stored phone key, `hello.client_id` must be the cloud id of that phone, and the stored psk of that phone must decrypt. The daemon never uses an offer psk on the direct path, so a pairing is never possible there. An unknown key, a removed phone, or a closed gate gets no message 2: the daemon closes the connection.
- The gate is the same as for the relay: phone access on and the plan has `remote_access`. When the gate closes, the daemon ends the direct sessions.
- The daemon side has a handshake limit of 5 seconds and at most 4 handshakes in flight from one remote IP. Direct sessions count toward the limits in "Sessions".

### Direct control record

After its `ready` record on a relay session, the daemon sends `{"type":"direct","v":1,"addrs":["192.168.1.10:7434","[fd7a:115c:a1e0::1]:7434"]}`. It sends it again when its addresses change. Each entry is `host:port`, and an IPv6 host is in brackets. The list has at most 8 entries. An empty list means that there is no direct path now. The daemon announces only the addresses that a phone on the same network can reach: RFC 1918, `100.64.0.0/10` (Tailscale and other VPNs), and IPv6 ULA. It never announces loopback or link-local addresses. The daemon sends no record when it has no direct listener. A phone that does not know the record ignores it, as for any control type.

The phone tries all addresses at the same time. It keeps the first that completes the handshake and the `ready` exchange, and it closes the others. Suggested limits on the phone: 300 ms to connect, 2 s for the handshake. The phone keeps the relay session open as the fallback. When the direct session fails, the phone uses the relay and tries direct again after the next `direct` record or a backoff.

`direct.v1.json` has framing cases (message to framed bytes), one stream of two framed messages read in small chunks, one `direct` control record sealed with the r2i cipher of `handshake.v1.json` (after its transport list), and bad cases for the reader and the writer.
