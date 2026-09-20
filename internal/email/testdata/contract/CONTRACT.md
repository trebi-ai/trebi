# trebi email Worker contract, version 1

This document is the published contract between `trebid` and a mail Worker. The fixtures `ingress-message.v1.json` and `send-request.v1.json` define the exact wire shapes. Anyone may implement a Worker against this contract. Both shapes carry a `v` field. A change to a shape is a coordinated release: update this repository first, then re-vendor in the Worker.

## Routes

All routes require `Authorization: Bearer <token>`. A missing or wrong token returns 401 with an empty body. There is no unauthenticated route, including health.

| Method | Route | Request | Response |
|---|---|---|---|
| `GET` | `/v1/ingress?after=<seq>&limit=<n>` | - | `{messages: [IngressMessage], next: <seq>}` |
| `POST` | `/v1/ingress/ack` | `{through: <seq>, known: [<local_part>]}` | 204, empty body |
| `POST` | `/v1/send` | `SendRequest` | `{message_id}`; 429 with `{retry_after_ms}`; errors `{error: {code, message}}` |
| `GET` | `/v1/attachments/{message}/{attachment}` | - | attachment bytes, 404 when absent |
| `GET` | `/v1/health` | - | `{version, spool_depth, spool_oldest_age_ms, known_count, counters}` |

`GET /v1/ingress` returns messages oldest first. `limit` is optional and caps the batch. `next` is the seq of the last message returned, or equals `after` when the response is empty. `POST /v1/ingress/stream` upgrades to a WebSocket; see "Stream" below.

## Semantics

`seq` is strictly monotonic within one Worker instance and is never reused. A gap in `seq` never occurs: the spool assigns the next number in the same order it stores the messages.

`ack` deletes every message with `seq <= through` and replaces the known set in one transaction. Ack is idempotent: a repeated ack with the same body changes nothing.

`known` holds local parts without tag. The Worker drops mail for an absent base only when the set is non-empty. An empty set means the Worker has no knowledge, so it spools every message. `trebid` sends the full live set with every ack.

`counters` counts `received`, `dropped_oversize`, `dropped_malformed_local_part`, `dropped_unknown_recipient`, `dropped_parse`, `sent`, and `send_failed`. Counters live for the life of the instance and never reset.

`POST /v1/send` requires `from` on the configured domain and at most 10 recipients across `to` and `cc`. The Worker sets `Message-ID` only when the caller omits it. `In-Reply-To` and `References` pass through unchanged. A 429 from the platform passes through as 429 with `retry_after_ms`. The Worker does not retry a send.

Errors use `{error: {code, message}}`. A validation failure is 400 with code `validation`. An unsupported `v` is a validation failure.

## Stream

`GET /v1/ingress/stream` upgrades to a WebSocket after the same bearer check. The Worker sends the text frame `mail` when it appends a message to the spool. The frame carries no content. The client pulls with `GET /v1/ingress`. Polling stays the correctness path; the stream is a latency optimisation.

## Versioning

`v` is 1. Add a field only in a coordinated release that updates both repositories. Never remove or repurpose a field. A Worker that receives a shape with a higher `v` rejects it with 400 `validation`.
