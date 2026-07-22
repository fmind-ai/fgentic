# Content-bounded identity audit (`infra/audit`)

The opt-in, regulated-deployment adapters that project **pinned** Synapse and MAS database rows into two Fgentic-owned, payload- and secret-free records ([ADR 0018](../../docs/adr/0018-content-bounded-identity-audit.md), issue #418). It exists so a regulated adopter can answer _"which Matrix identity authenticated successfully?"_ and _"which non-rejected event did this homeserver persist?"_ without treating generic application logs as evidence.

This directory holds the **complete offline collector logic** — the compliance-critical half of #418, fully testable without a cluster: closed schemas, fail-closed projectors, the cursor/dedup reconcile, the read-only column-level grants, and the crash-safe cycle orchestration. Only the concrete runtime **adapters** remain (a Postgres `Execute`, the [#157](https://github.com/fmind-ai/fgentic/issues/157) durable sink, the cursor store) plus the `on_new_event` wake-up and the opt-in Kustomize component.

## What is here

- `schemas/` — the closed JSON Schemas: `fgentic.matrix_event.v1` and `fgentic.mas_authentication.v1` (this feature) and `fgentic.admin_action.v1` (the sibling privileged-admin-action stream, issue #455). `additionalProperties: false` and a fixed `required` set make the field sets closed; adding or renaming a field requires a new schema version.
- `base/records.py` — pure projectors that turn one exact-version source row into a frozen closed record or **fail closed**. They read only explicitly named, allowlisted columns, so a payload, credential, IP, User-Agent, token, or any unlisted column is never read and can never reach the output. A row whose column set does not match the pinned fingerprint (a version drift) is rejected before any field is read.
- `base/reconcile.py` — the cursor + dedup reconciliation: advances a durable `stream_ordering` high-water past emitted/suppressed rows, deduplicates by `(schema, event_id)` / `(schema, authentication_id)`, and fails closed on a retrograde cursor.
- `base/collector.py` — the pinned SQL the read-only roles execute (selecting only the allowlisted columns) plus the orchestration wiring each parameterised query batch through the projectors and reconcile via an injected `execute` callable.
- `base/cycle.py` — the crash-safe reconciliation cycle: records are written to the sink **before** the cursor advances, so a crash between them re-fetches under dedup; any failure aborts before the cursor saves. The sink and cursor store are injected `Protocol`s.
- `base/schemacheck.py` — a dependency-free closed-schema validator shared by the contract tests.
- `sql/read-only-roles.sql` — the `NOLOGIN` collector roles with column-level `SELECT` grants on exactly the allowlisted columns; a governance test ties them to the projector allowlist so a widened or whole-table grant fails the gate.
- `tests/` — the ADR's negative gates and the collector/reconcile/cycle/grant contracts as deterministic fixtures (129 tests, no Kubernetes runtime): closed field sets, structural exclusion of every forbidden field, rejected/outlier suppression, single-method selection, cursor/dedup and crash-safe ordering, and fail-closed behaviour on version drift, missing columns, ambiguous methods, malformed localparts, and widened grants.

Run it with `mise run check:identity-audit` (it reuses the pinned `synapse-federation-policy` toolchain — ruff, ty, pytest). The Synapse and MAS queries, the projector column allowlists, and the read-only grants are additionally validated live against the demo cluster's exact-version Synapse 1.155.0 / MAS 1.19.0 schema.

## Claims and non-claims

- `fgentic.matrix_event.v1` proves Synapse persisted a non-rejected timeline/state event **locally**. It does **not** identify the device, MAS session, access token, HTTP request, or whether another homeserver retains the event.
- `fgentic.mas_authentication.v1` proves one MAS authentication **committed successfully**. It does **not** represent a failed attempt, token issuance, consent, or a later Matrix API request. Failed MAS attempts are deliberately unattributed; a username echoed by a failed attempt is never turned into an authenticated identity.
- The `matrix_user`↔`sender` join proves the same Matrix identity authenticated and later appears as an event sender; **timestamps alone do not prove** a particular session or token submitted that event.
- "Content-bounded" means payload- and secret-free, **not anonymous**: authentication IDs, session IDs, MXIDs, room IDs, and event IDs remain personal/linkable operational data and require the operator/auditor-only access and 90-day deletion controls the live component adds.

## Version pinning

The projectors are pinned to the ESS 26.6.2 reference: Synapse `v1.155.0` `events` and MAS `v1.19.0` `user_session_authentications`. The source-column allowlists in `base/records.py` are the schema fingerprint; an upgrade that changes them fails the offline contract until the fixtures are re-proved against the new migration set, per ADR 0018.
