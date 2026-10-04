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

## Socket frames, daemon to cloud

| Type | Fixture | Fields |
|---|---|---|
| `hello` | `hello.v1.json` | `v`, `type`, `caps` |
| `heartbeat` | `heartbeat.v1.json` | `v`, `type` |
| `alert` | `alert.v1.json` | `v`, `type`, `cursor`, `kind`, `summary`, `message_id` |
| `ack` | `ack.v1.json` | `v`, `type`, `id` |
| `relay_frame` | `relay-frame.v1.json` | `v`, `type`, `bytes` |
| `mcp_response` | `mcp-response.v1.json` | `v`, `type`, `id`, `status`, `headers`, `body` |

`hello.caps` is optional. An absent or empty list means no remote MCP. `alert.summary` is notification text, not a run log. The daemon mints and stores `message_id` before it sends the frame. `ack.id` is the ingress id. `relay_frame.bytes` is opaque. The cloud does not decode it.

## Socket frames, cloud to daemon

| Type | Fixture | Fields |
|---|---|---|
| `hello_ok` | `hello-ok.v1.json` | `v`, `type`, `plan`, `features`, `heartbeat_interval`, `mcp_url` |
| `ingress` | `ingress.v1.json` | `v`, `type`, `id`, `kind`, `payload` |
| `relay_frame` | `relay-frame.v1.json` | `v`, `type`, `bytes` |
| `entitlement` | `entitlement.v1.json` | `v`, `type`, `plan`, `features` |
| `mcp_request` | `mcp-request.v1.json` | `v`, `type`, `id`, `client`, `scope`, `headers`, `body` |

`hello_ok.features` is a cache hint. The worker checks the plan again before every granted action. Kind `email` carries `ingress-message.v1.json`. Kind `webhook` carries `webhook-ingress.v1.json`. An approval reply is an `ingress` frame of kind `email`. There is no decision frame.

An unknown `type` is ignored and counted. The cloud does not execute a job.

## Remote MCP

The cloud forwards one MCP HTTP POST from a remote client (Claude, ChatGPT) as one `mcp_request`. The daemon answers with exactly one `mcp_response` with the same `id`. A response with an unknown `id` is dropped and counted. The cloud sends `mcp_request` only when the last `hello` had the cap `mcp`.

- `headers` keys are lowercase. The cloud sends only these keys: `content-type`, `accept`, `mcp-protocol-version`, `mcp-method`, `mcp-name`, `mcp-session-id`. The daemon ignores all other keys.
- `body` is the raw HTTP body as a UTF-8 string, at most 4 MiB in each direction.
- `client` is the OAuth client id. `scope` is the granted scope list: `mcp:read`, and `mcp:write` for full control.
- `hello_ok.mcp_url` is the URL that the user gives to the client. The daemon shows it and does not build it.
- The cloud does not store or log `body`. It reads only the JSON-RPC `id` and `method`.
- An error that the daemon or the cloud builds is HTTP 200 with a JSON-RPC error that carries the request `id`. A notification gets 202 with no body. The codes are `-32001` machine offline, `-32002` remote MCP off, `-32003` timeout, and `-32004` busy.

## Webhook ingress

`webhook-ingress.v1.json` holds `v`, `id`, `method`, `path`, `content_type`, `headers`, `body`, and `received_at`. `headers` is an allowlist so the daemon can verify a sender signature. The cloud does not parse `body` into a job.

## Versioning

`v` is 1. Add a field only in a coordinated release that updates both repositories. Never remove or repurpose a field.
