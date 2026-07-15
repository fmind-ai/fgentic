---
type: Architecture Decision Record
title: Content-Bounded Matrix Identity Audit
description: Source optional Matrix authentication and event evidence from pinned databases, not generic logs.
---

# 0018 — Content-Bounded Matrix Identity Audit

Status: Accepted

Implementation: #300 owns the reference-IdP event contract; #157 owns the optional durable query store; #418 owns the Synapse/MAS adapters and the executable schemas and negative gates in this ADR.

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

## Consequences

1. A regulated deployment can answer “which Matrix identity authenticated successfully?” and “which non-rejected event did this homeserver persist?” without treating arbitrary application logs as evidence.
1. The design does not overclaim session-to-event causality or named failed-login attribution. Those gaps remain visible rather than being filled with time correlation.
1. Private schema coupling creates deliberate upgrade work, isolated behind an opt-in component and exact-version fixtures. Unmodified upstream images and a separate adapter preserve the repository's AGPL boundary.
1. The retained identifiers require explicit access and deletion controls. “Content-bounded” means payload- and secret-free, not anonymous.
