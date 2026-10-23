# trebi cloudlink contract, version 1

This document is the published contract between `trebid` and the hosted workers. The fixtures in this directory define the exact wire shapes. Hosted mail reuses `ingress-message.v1.json` and `send-request.v1.json` from the email contract unchanged. Every shape carries a `v` field. A change to a shape is a coordinated release: update this repository first, then re-vendor in `trebi-cloud`.

## Transport

The daemon dials out. There is no inbound port on the machine. Sockets carry push. Request and response go over signed HTTP.

The device public key is the hosted credential. The daemon mints a keypair at link time and sends the raw Ed25519 public key, standard base64, 32 bytes. The private key stays on the machine.

Signed calls send four headers:

| Header | Value |
|---|---|
| `X-Trebi-Device` | device id |
| `X-Trebi-Timestamp` | unix milliseconds, decimal |
| `X-Trebi-Nonce` | unique string, no CR, LF, or NUL |
| `X-Trebi-Signature` | standard base64 of the Ed25519 signature |

The signed payload is `timestamp` + LF + `nonce` + LF + `METHOD` + LF + `pathname`. `METHOD` is uppercase. `pathname` has no query and no host. The worker rejects a timestamp outside five minutes, a reused nonce, and a signature that does not verify. A failed check is 401 with an empty body. `hello` carries no second signature. The upgrade request carries the signature.

## Addresses

The worker serves the same routes on each of its addresses: the main host (`staging-api.trebi.ai`, later `api.trebi.ai`) and its `workers.dev` address (`trebi-cloud-api-staging.ocral.workers.dev`, later `trebi-cloud-api.ocral.workers.dev`). Only the MCP host has its own routes. The signed payload has no host, so the worker accepts the same signed request on each address. `workers_dev` must stay on in `wrangler.jsonc`.

A network can block the Cloudflare IPs of `trebi.ai` and leave the `workers.dev` address open. The daemon reads its addresses from `cloud.api_url` and `cloud.fallback_urls`. It moves to the next address only when the connection does not open: a dial error, a DNS error, or a TLS handshake error. The connect timeout is 8 seconds. An HTTP answer of any status never moves the address. A timeout after the request was sent never moves the address, because `POST /v1/send` is not idempotent. The daemon signs each try again with a new nonce. It keeps the backup address and sends `GET /health` to the main address each 10 minutes. When the main address answers, the daemon moves back. The socket moves back at its next dial. The pairing link always names the main host. `internal/relay/testdata/contract/CONTRACT.md`, "Backup address", has the phone side.

## Socket frames, daemon to cloud

| Type | Fixture | Fields |
|---|---|---|
| `hello` | `hello.v1.json` | `v`, `type`, `caps` |
| `heartbeat` | `heartbeat.v1.json` | `v`, `type` |
| `alert` | `alert.v1.json` | `v`, `type`, `cursor`, `kind`, `summary`, `message_id` |
| `ack` | `ack.v1.json` | `v`, `type`, `id` |
| `relay_frame` | `relay-frame.v1.json` | `v`, `type`, `sid`, `bytes` |
| `relay_close` | `relay-close.v1.json` | `v`, `type`, `sid`, `reason` |
| `mcp_response` | `mcp-response.v1.json` | `v`, `type`, `id`, `status`, `headers`, `body` |
| `login_result` | `login-result.v1.json` | `v`, `type`, `id`, `status`, `title`, `message` |

`hello.caps` is optional. An absent or empty list means no remote MCP and no phone access. The cap `mcp` turns on remote MCP. The cap `mobile` turns on phone access. `alert.summary` is notification text, not a run log. `alert.kind` is `approval` or `request` (`alert-request.v1.json`). The cloud sends a push for both kinds. `message_id` is the inbox item id. The daemon stores the item before it sends the frame. The push opens `/inbox/<message_id>` in the app. `ack.id` is the ingress id. See "Relay" for `relay_frame` and `relay_close`.

## Socket frames, cloud to daemon

| Type | Fixture | Fields |
|---|---|---|
| `hello_ok` | `hello-ok.v1.json` | `v`, `type`, `plan`, `features`, `heartbeat_interval`, `mcp_url` |
| `ingress` | `ingress.v1.json` | `v`, `type`, `id`, `kind`, `payload` |
| `relay_open` | `relay-open.v1.json` | `v`, `type`, `sid`, `client_id` |
| `relay_frame` | `relay-frame.v1.json` | `v`, `type`, `sid`, `bytes` |
| `relay_close` | `relay-close.v1.json` | `v`, `type`, `sid`, `reason` |
| `client_revoked` | `client-revoked.v1.json` | `v`, `type`, `client_id` |
| `entitlement` | `entitlement.v1.json` | `v`, `type`, `plan`, `features` |
| `mcp_request` | `mcp-request.v1.json` | `v`, `type`, `id`, `client`, `scope`, `headers`, `body` |
| `login_return` | `login-return.v1.json` | `v`, `type`, `id`, `state`, `code`, `iss`, `error`, `error_description` |

`hello_ok.features` is a cache hint. The worker checks the plan again before every granted action. Kind `email` carries `ingress-message.v1.json`. Kind `webhook` carries `webhook-ingress.v1.json`. An approval reply is an `ingress` frame of kind `email`. There is no decision frame.

An unknown `type` is ignored and counted. The cloud does not execute a job.

## Relay

The relay carries the traffic between a paired phone and the daemon. The phone and the daemon encrypt it end to end with `trebi-relay/1` (the daemon repository, `internal/relay/testdata/contract/CONTRACT.md`). The cloud sees only ciphertext. It routes frames by `sid` and never decodes, logs, or stores `bytes`.

- `sid` is a routing id that the cloud mints for each phone socket (26 chars, ULID). It is not a secret.
- `client_id` is the cloud id of the phone. The daemon matches it with the `client_id` in the encrypted handshake.
- The cloud sends `relay_open` when a phone socket opens, and `relay_close` with reason `client_gone` when it closes.
- `relay_frame.bytes` is the standard base64 of one Noise message, at most 65,536 chars. The cloud forwards a phone message to the daemon as `relay_frame{sid}` and a daemon `relay_frame{sid}` to that phone socket only. A frame with an unknown `sid` is dropped and counted.
- The daemon sends `relay_close` to end one phone socket. The reasons are `refused` (the handshake failed), `limit`, `revoked`, and `daemon_off`. The cloud closes the phone socket with `4409` for `refused` and `1000` for all other reasons.
- `client_revoked` tells the daemon that a phone was removed in the cloud. The daemon deletes that phone.
- The cloud sends `relay_open` only when the last `hello` had the cap `mobile`.

### Phone socket

The phone opens `GET /v1/relay` with `Authorization: Bearer trc_…`. Each WebSocket text message is the standard base64 of one Noise message. Binary messages are dropped.

| Upgrade error | Code | Cause |
|---|---|---|
| `401` | `bad_token` | No token, or the token is not known |
| `403` | `remote_access_required` | The plan has no `remote_access` |
| `410` | `client_revoked` | The phone was removed |
| `503` | `mobile_off` | The daemon socket has no cap `mobile` |
| `429` | `too_many_sessions` | More than 8 phone sockets for one machine |

| Close code | Meaning |
|---|---|
| `4404` | `daemon_offline`: no daemon socket, or it closed |
| `4409` | `refused`: the daemon refused the handshake |
| `4410` | `revoked`: the phone was removed |
| `4403` | `plan_changed`: the plan lost `remote_access` |

### Phone API

`phone-api.v1.json` holds the HTTP shapes between the phone and the cloud.

| Method | Route | Auth | Body | Reply |
|---|---|---|---|---|
| `POST` | `/v1/clients/claims` | device signature | `claim-mint-request.v1.json` | `claim-mint-response.v1.json` |
| `POST` | `/v1/clients` | claim in the body | `enroll_request` | `201 enroll_response` |
| `DELETE` | `/v1/clients/{id}` | device signature or session | | `204` |
| `PUT` | `/v1/clients/me/push-token` | phone token | `push_token_request` | `204` |
| `DELETE` | `/v1/clients/me` | phone token | | `204` |

- A claim is good for `uses` enrollments (1 to 10) and for `ttl_seconds` (60 to 2,592,000). The cloud stores only its SHA-256.
- A phone token is `trc_` plus 32 random bytes in base64url. The cloud stores only its SHA-256.
- `platform` is `ios` or `android`. `provider` is `apns` or `fcm`. `environment` is `sandbox` or `production`.
- An error is `error_response`. The codes are `bad_claim`, `bad_token`, `rate_limited`, `remote_access_required`, `client_revoked`, and `validation`.

## Remote MCP

The cloud forwards one MCP HTTP POST from a remote client (Claude, ChatGPT) as one `mcp_request`. The daemon answers with exactly one `mcp_response` with the same `id`. A response with an unknown `id` is dropped and counted. The cloud sends `mcp_request` only when the last `hello` had the cap `mcp`.

- `headers` keys are lowercase. The cloud sends only these keys: `content-type`, `accept`, `mcp-protocol-version`, `mcp-method`, `mcp-name`, `mcp-session-id`. The daemon ignores all other keys.
- `body` is the raw HTTP body as a UTF-8 string, at most 4 MiB in each direction.
- `client` is the OAuth client id. `scope` is the granted scope list: `mcp:read`, and `mcp:write` for full control.
- `hello_ok.mcp_url` is the URL that the user gives to the client. The daemon shows it and does not build it.
- The cloud does not store or log `body`. It reads only the JSON-RPC `id` and `method`.
- An error that the daemon or the cloud builds is HTTP 200 with a JSON-RPC error that carries the request `id`. A notification gets 202 with no body. The codes are `-32001` machine offline, `-32002` remote MCP off, `-32003` timeout, and `-32004` busy.

## Login return

The daemon is the OAuth client of an online MCP server. When the daemon cannot use a return page on this computer, the redirect URI is `https://api.trebi.ai/login/done`. The cloud sends one `login_return` for each hit on that page. The daemon answers with exactly one `login_result` with the same `id`.

- `state` is `<device id>.<random>`. The cloud reads the device id before the first dot and sends the frame only to that device. A state with an unknown device gets the "unknown login" page and no frame.
- `code`, `iss`, `error`, and `error_description` are the query values of the return. Each is optional. The cloud does not store or log `code`.
- `login_result.status` is `done`, `failed`, or `gone`. `done` carries the connection `title`. `failed` carries a `message` for the person. `gone` means that no open login has the state.
- The cloud waits 15 seconds for `login_result`. When the device is offline or does not answer, the page tells the person to paste the page address into Trebi. The cloud sends `login_return` only to a device with remote access.
- The OAuth client id of the daemon is the CIMD document `https://api.trebi.ai/oauth/client.json`. The cloud serves it as a static file.

## Webhook ingress

`webhook-ingress.v1.json` holds `v`, `id`, `hook_id`, `method`, `path`, `query`, `content_type`, `headers`, `body`, `received_at`, and `handshake`. `id` is the payload id; the daemon uses it to remove a repeated delivery. `hook_id` is the id of the hook that got the request. `query` is the raw query string without `?`. The cloud does not parse `body` into a job.

`headers` is an allowlist so the daemon can verify a sender signature. The cloud forwards a fixed base list plus the names that the hook declared at mint time.

`POST /v1/hooks` takes `hook-mint-request.v1.json`: `label`, plus the optional `headers` and `handshake`. It answers `hook-mint-response.v1.json`: `id`, `token`, and `url`. A header name matches `^[a-z0-9-]{1,64}$`; there are at most 16. The cloud refuses `authorization`, `cookie`, `set-cookie`, `proxy-authorization`, and each name that starts with `cf-` or `x-forwarded-`.

The `handshake` rules are `query_echo:<param>`, `json_echo:<field>`, `header_echo:<header>`, and `ok:GET` or `ok:HEAD`. There are at most 4. The cloud tries them in order, and the first match answers:

- `query_echo:<param>`: the query has `<param>`. Answer 200 `text/plain` with the value.
- `json_echo:<field>`: the body is a JSON object with a string `<field>` and no other key except `type` and `token`. Answer 200 `application/json` `{"<field>": value}`.
- `header_echo:<header>`: the request has `<header>`. Answer 200 with the same header and value, and no body.
- `ok:GET`, `ok:HEAD`: the method matches and no echo rule matched. Answer 200 with no body.

The cloud forwards an echo match with `handshake: true`. It does not forward an `ok` match. A request that matches no rule is a normal delivery: its method must be `POST`, `PUT`, or `PATCH`, else the cloud answers 405. The cloud answers a normal delivery with 204 and buffers it. `hook-handshake.v1.json` holds the cases that the cloud and the daemon both run.

## Versioning

`v` is 1. Add a field only in a coordinated release that updates both repositories. Never remove or repurpose a field.
