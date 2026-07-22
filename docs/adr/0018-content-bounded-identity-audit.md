---
type: Architecture Decision Record
title: Content-Bounded Matrix Identity Audit
description: Source optional Matrix authentication and event evidence from pinned databases, not generic logs.
---

# 0018 — Content-Bounded Matrix Identity Audit

Status: Accepted

Decision register: [D19](../design-decisions.md)

Implementation: #300 owns the reference-IdP event contract; #157 owns the optional durable query store; #418 owns the Synapse/MAS authentication and event adapters and the executable schemas and negative gates in this ADR; #455 owns the third stream — the privileged admin-action adapter recorded in [Admin-action audit (#455)](#admin-action-audit-455) below, built to the same discipline.

## Context

The stable bridge audit proves which Matrix sender delegated to which Agent after Synapse accepted an event. It does not prove how that user authenticated or provide a homeserver-wide record of accepted Matrix events. Generic logs cannot fill that gap safely or reliably.

The [reference stack pins ESS 26.6.2](../../infra/flux/sources.yaml), whose chart pins Synapse `v1.155.0` and Matrix Authentication Service (MAS) `v1.19.0`. Inspection of those exact sources found three materially different surfaces:

1. Synapse [documents `on_new_event`](https://github.com/element-hq/synapse/blob/v1.155.0/docs/modules/third_party_rules_callbacks.md#on_new_event) as running after an event is processed and stored and never for rejected events. It is a supported module callback, but it runs on every worker and [callback failures are logged and swallowed](https://github.com/element-hq/synapse/blob/v1.155.0/synapse/module_api/callbacks/third_party_event_rules_callbacks.py#L408-L434). It is therefore an at-least-once wake-up signal, not a complete durable audit source.
1. Synapse's private [`events` table](https://github.com/element-hq/synapse/blob/v1.155.0/synapse/storage/schema/main/full_schemas/72/full.sql.postgres#L409-L427) carries the content-free persistence tuple beside payload columns: event ID, type, room, sender, timestamps, stream position, outlier flag, and rejection reason. A pinned query can select only that tuple and reconcile missed callback output without reading `content` or `unrecognized_keys`.
1. MAS [request telemetry](https://github.com/element-hq/matrix-authentication-service/blob/v1.19.0/crates/cli/src/server.rs#L97-L215) records route, path, query, status, and User-Agent, while the [login handlers use `skip_all`](https://github.com/element-hq/matrix-authentication-service/blob/v1.19.0/crates/handlers/src/views/login.rs#L133); it has no documented authentication-event callback. Those records identify neither the authenticated user nor a stable outcome and include fields that do not belong in a bounded audit stream. MAS instead exposes separate successful password and upstream-OIDC writes to [`user_session_authentications`](https://github.com/element-hq/matrix-authentication-service/blob/v1.19.0/crates/storage-pg/src/user/session.rs#L447-L543). Its v1 Admin API exposes user sessions, but not the authentication row or method needed for this claim.

The MAS and Synapse database layouts are private implementation schemas, not public APIs. They can support an exact-version opt-in profile, but they cannot become an unversioned platform promise. Keycloak events are a fourth source owned by #300: they prove the reference upstream IdP's outcome, not that MAS subsequently linked a Matrix identity or created a session.

## Decision

1. Authentication/event audit is an **opt-in regulated-deployment component**, disabled in every default and evaluation profile. The base platform continues to promise only the bridge, MCP, PostgreSQL DDL/ROLE, and Kubernetes audit boundaries already documented. Enabling the component requires the durable, authenticated query and retention boundary from #157; raw pod logs are not the product surface.
1. The component emits exactly two Fgentic-owned schemas. Their field sets are closed; adding or renaming a field requires a new schema version.

   | Schema                          | Authoritative source                                                                                                             | Required fields                                                                                            | Claim and limit                                                                                                                                                                           |
   | ------------------------------- | -------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
   | `fgentic.mas_authentication.v1` | MAS 1.19.0 `user_session_authentications` joined to `user_sessions`, `users`, and the selected authentication-method foreign key | `occurred_at`, `authentication_id`, `session_id`, `matrix_user`, `method`, optional `upstream_provider_id` | One MAS authentication committed successfully. It does not represent a failed attempt, token issuance, consent, or a later Matrix API request.                                            |
   | `fgentic.matrix_event.v1`       | Synapse 1.155.0 `events`, filtered to non-outlier rows with a stream position and no rejection reason                            | `origin_server_ts`, `received_ts`, `event_id`, `room_id`, `sender`, `event_type`, `stream_ordering`        | Synapse persisted a non-rejected timeline/state event locally. It does not identify the device, MAS session, access token, HTTP request, or whether another homeserver retains the event. |

1. `matrix_user` is the full `@localpart:server_name` reconstructed from the MAS username and configured server name. It joins exactly to a local Synapse `sender`. The join proves that the same Matrix identity authenticated and later appears as an event sender; timestamps alone do **not** prove that a particular session or token submitted that event. Authentication IDs, session IDs, MXIDs, room IDs, and event IDs remain personal or linkable operational data even though they are not profile PII.
1. The collectors use dedicated read-only database roles with column-level grants for the exact selected columns. They never receive `SELECT` on event content, unrecognized event keys, password hashes, email addresses, IP addresses, User-Agent values, OAuth codes, access/refresh tokens, redirect URIs, or encrypted client secrets. A schema mismatch, ambiguous authentication method, missing localpart, malformed MXID, pagination/high-water regression, or source query error fails the collection cycle without emitting a partial record.
1. `on_new_event` may wake the Synapse adapter, but the private database cursor is the reconciliation source. The sink deduplicates Matrix records by `(schema,event_id)` and MAS records by `(schema,authentication_id)`. A callback invocation count is never presented as an accepted-event count.
1. MAS authentication failures remain deliberately unattributed. The aggregate `mas.user.password_login_attempt{result=...}` metric may support rate monitoring, and #300 may emit bounded Keycloak `LOGIN_ERROR` evidence for the reference upstream IdP, but neither proves a failed MAS-to-Matrix authentication for a named user. TerseJson, MAS access logs, trace spans, HTTP status, and time-window correlation are explicitly rejected as substitutes.
1. Retention defaults to 90 days, matching the accepted #300 identity-event policy, and can only be shortened without another review. Query access is limited to the operator and auditor roles. Deletion must remove expired projected records without altering the MAS or Synapse sources of record; source-system retention and backup erasure remain separate policies.
1. Implementation must pin the exact ESS, Synapse, MAS, and database-schema fingerprints. An upgrade fails its offline contract until fixtures prove both private queries against the new migration set. A supported upstream audit API can replace the MAS database adapter only after a non-prerelease release provides a versioned schema, durable pagination/cursor semantics, and the same negative-content guarantees.

## Required negative gates

Before an implementation issue can be considered complete, deterministic fixtures must prove:

1. Event body, formatted body, state content, unsigned data, transaction IDs, passwords, hashes, emails, display names, IPs, User-Agent values, cookies, authorization codes, access/refresh tokens, request queries, redirect URIs, and client secrets never enter either record.
1. The output contains the exact allowlisted keys and enum values; unknown source fields cannot pass through automatically.
1. Rejected and outlier Synapse rows do not emit, duplicate callback delivery emits once, and a missed callback is recovered once from the stream cursor.
1. Failed MAS attempts do not manufacture an identity-bearing record; password and upstream-OIDC success fixtures select exactly one authentication method.
1. Wrong source versions, missing columns, duplicate/retrograde cursors, malformed localparts, and database errors fail closed.
1. The 90-day deletion boundary and operator/auditor authorization are enforced, and disabling the component leaves no collector, database role, query surface, dashboard, Secret, or retained audit data.

## Upstream request draft and release gate

Publishing under the maintainer's identity remains a human action. The prepared MAS request is:

> Provide a supported, versioned authentication-audit stream or callback emitted after the authentication transaction commits. It needs an opaque event ID, occurrence time, outcome, method, and user/session IDs only when known; durable cursor semantics; explicit field-version ownership; and a guarantee that credentials, OAuth codes/tokens, cookies, request URIs/queries, emails, IPs, and User-Agent values are absent. Failed attempts must not echo an unverified username as an authenticated identity.

No current implementation depends on that request being accepted. Track the upstream issue and first stable release in the implementation issue once the maintainer publishes it; retain the pinned private-schema adapter until the supported replacement passes the same fixtures. Synapse already supplies the documented post-persistence callback, so no Synapse request is a prerequisite; a future durable cursor API could replace its private reconciliation query under the same gate.

## Admin-action audit (#455)

A regulated adopter's auditor asks the converse of the authentication question with teeth: _who_ suspended this user, purged this room, quarantined this media, or dismissed this report, _when_, and with what outcome. Enabling the Ketesa admin console (#135) widens a privileged surface, so the same content-bounded discipline is extended to a third stream, `fgentic.admin_action.v1`. Its offline core (projector, cursor/dedup reconcile, crash-safe cycle, and negative gates) lives beside the other two in `infra/audit`; the runtime log adapter, the opt-in component, and the #157 sink write are deferred exactly as the #418 runtime adapters are.

### Capture point, with evidence

1. The authoritative source is the **pinned Synapse 1.155.0 `SynapseRequest` completion access log** — the line emitted by [`_finished_processing`](https://github.com/element-hq/synapse/blob/v1.155.0/synapse/http/site.py) — projected to the authenticated requester, HTTP method, redacted admin route, and status. It is the _only_ source that resolves an admin's opaque bearer token to their MXID: Synapse computes the authenticated entity (`{@admin:server}`) while processing the request. The **gateway/Traefik access log cannot attribute the action** — it sees an opaque `Authorization: Bearer …` header (or nothing, under MSC3861 the token is validated by MAS), never the MXID, and resolving it would require an out-of-band introspection the reference deliberately does not add. agentgateway is not in the Synapse admin request path at all.
1. The access-log **format string is the version fingerprint**, exactly as the DB column set is for the other two streams: `"%s - %s - {%s} Processed request: … %sB %s \"%s %s %s\" \"%s\" …"`. The projector's parse (`collector.ADMIN_ACCESS_LOG_PATTERN`) binds only the authenticated entity, status, method, and redacted path. The line _also carries the client IP (leading field) and the User-Agent (trailing quoted field) inline_ — neither is ever bound to a capture group, so they cannot reach a record. A Synapse whose format string drifts no longer matches and the parse fails closed.
1. This is a **structured LOG-LINE projection, not a database query**, so unlike the #418 streams there is **no read-only database role or column grant** — the source is a log handler the opt-in component configures, not a table. The offline projector consumes the already-parsed structured row (`occurred_at`, `acting_entity`, `method`, `path`, `status`, and a monotonic ingest `position`); the runtime adapter that tails the pinned `log_config` JSON handler and assigns `position` is the deferred half.

### Closed record and the pinned route table

`fgentic.admin_action.v1` is closed with `additionalProperties: false`: `occurred_at`, `acting_admin` (a full MXID), `action_class` (a fixed enum), `target` (a bounded operational identifier), and `outcome` (`succeeded` | `failed` | `denied`). `position` is the reconcile cursor/dedup key on the dataclass and is deliberately **not** part of the wire record. Outcome maps from status: `2xx` → succeeded, `403` → denied, everything else → failed.

The projector emits a record only for the pinned v1.155.0 admin mutation routes whose action class **and** a content-free, non-secret target are fully determined by the request line alone:

| Action class       | Pinned route(s)                                                                                                                                    | Target                 |
| ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------- |
| `room_purge`       | `DELETE /_synapse/admin/v1/rooms/<room_id>`, `DELETE /_synapse/admin/v2/rooms/<room_id>`                                                           | room id                |
| `media_quarantine` | `POST /_synapse/admin/v1/media/quarantine/<server>/<media_id>`, `POST …/room/<room_id>/media/quarantine`, `POST …/user/<user_id>/media/quarantine` | media / room / user id |
| `report_dismiss`   | `DELETE /_synapse/admin/v1/event_reports/<report_id>`                                                                                              | report id              |

### Non-claims (must survive a skeptical DPO reading)

1. **Suspend vs reactivate direction is not asserted.** Account suspension is a single endpoint, `PUT /_synapse/admin/v1/suspend/<user_id>`, whose direction lives only in the request body `{"suspend": true|false}` — a _request argument_ the content-free record structurally excludes. Emitting `user_suspend` vs `user_reactivate` from the request line would be a guess, so this stream does not emit it; the `user_suspend`/`user_reactivate` enum values remain reserved for a future body-aware capture. This is a documented boundary, not a silent gap: the projector returns no record for the suspend route.
1. **Registration-token actions are not attributed.** `registration_token_revoke` puts the secret token itself in the URL (`DELETE …/registration_tokens/<token>`) and `registration_token_issue` returns the new token only in the response body; neither yields a content-free, non-secret target from the request line, so both are scoped out (the token action is covered by this documented non-claim).
1. **MAS-plane admin actions are not attributed.** As established for authentication above, MAS 1.19.0 request telemetry identifies neither the authenticated user nor a stable outcome; MAS admin-API session termination and token operations have no attributable source at the pinned version and are not inferred from timing or generic telemetry.
1. **Only admin-API calls are seen.** Actions performed by a direct database operator or via `kubectl` never traverse the Synapse admin API and are out of scope; this stream attributes an admin-API _call_, it does not authenticate _intent_. Unauthenticated (`401`) and puppeted/appservice (`authenticated_entity|requester`) requests are not attributed to a single human admin and emit nothing.
1. **Denied non-admin attempts are recorded, not dropped.** A non-admin who is authenticated but unauthorised receives `403` with their own MXID, which is recorded with `outcome: denied` — no silent gap.

### Required negative gates (admin stream)

Deterministic fixtures (`infra/audit/tests/test_admin_*.py`) prove, without a runtime: the closed field set and structural exclusion of every payload/credential/network field; the action-class and outcome enums against real projected instances; that the client IP and User-Agent present in the raw line are never captured; that each audited route maps to its class and target; that denials are recorded; that suspend/reactivate, token, read, unauthenticated, and puppeted rows emit nothing; the monotonic `position` cursor/dedup and crash-safe write-before-cursor ordering; and fail-closed behaviour on format/version drift, missing/extra columns, malformed MXIDs, and invalid status/position. The opt-in component (zero footprint in default/demo/federation renders), the `log_config` and log adapter, the #157 sink write, and the live Ketesa proof are the deferred remainder.

## Consequences

1. A regulated deployment can answer “which Matrix identity authenticated successfully?”, “which non-rejected event did this homeserver persist?”, and “which admin performed this privileged mutation, and with what outcome?” without treating arbitrary application logs as evidence.
1. The design does not overclaim session-to-event causality or named failed-login attribution. Those gaps remain visible rather than being filled with time correlation.
1. Private schema coupling creates deliberate upgrade work, isolated behind an opt-in component and exact-version fixtures. Unmodified upstream images and a separate adapter preserve the repository's AGPL boundary.
1. The retained identifiers require explicit access and deletion controls. “Content-bounded” means payload- and secret-free, not anonymous.
