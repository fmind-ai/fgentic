# Content-bounded identity audit (`infra/audit`)

The opt-in, regulated-deployment adapters that project **pinned** Synapse and MAS database rows into two Fgentic-owned, payload- and secret-free records ([ADR 0018](../../docs/adr/0018-content-bounded-identity-audit.md), issue #418). It exists so a regulated adopter can answer _"which Matrix identity authenticated successfully?"_ and _"which non-rejected event did this homeserver persist?"_ without treating generic application logs as evidence.

This directory currently holds the **closed-schema contract + fail-closed projection layer** — the compliance-critical heart, fully testable offline. The live collector (read-only DB roles, the `on_new_event` wake-up, the durable cursor, the [#157](https://github.com/fmind-ai/fgentic/issues/157) sink) and the opt-in Kustomize component are the remaining #418 tasks.

## What is here

- `schemas/` — the two closed JSON Schemas (`fgentic.matrix_event.v1`, `fgentic.mas_authentication.v1`). `additionalProperties: false` and a fixed `required` set make the field sets closed; adding or renaming a field requires a new schema version.
- `base/records.py` — pure projectors that turn one exact-version source row into a frozen closed record or **fail closed**. They read only explicitly named, allowlisted columns, so a payload, credential, IP, User-Agent, token, or any unlisted column is never read and can never reach the output. A row whose column set does not match the pinned fingerprint (a version drift) is rejected before any field is read.
- `tests/test_records.py` — the ADR's required negative gates as deterministic fixtures (no Kubernetes runtime): closed field sets, structural exclusion of every forbidden field, rejected/outlier suppression, single-method selection, and fail-closed behaviour on version drift, missing columns, ambiguous methods, and malformed localparts.

Run it with `mise run check:identity-audit` (it reuses the pinned `synapse-federation-policy` toolchain — ruff, ty, pytest).

## Claims and non-claims

- `fgentic.matrix_event.v1` proves Synapse persisted a non-rejected timeline/state event **locally**. It does **not** identify the device, MAS session, access token, HTTP request, or whether another homeserver retains the event.
- `fgentic.mas_authentication.v1` proves one MAS authentication **committed successfully**. It does **not** represent a failed attempt, token issuance, consent, or a later Matrix API request. Failed MAS attempts are deliberately unattributed; a username echoed by a failed attempt is never turned into an authenticated identity.
- The `matrix_user`↔`sender` join proves the same Matrix identity authenticated and later appears as an event sender; **timestamps alone do not prove** a particular session or token submitted that event.
- "Content-bounded" means payload- and secret-free, **not anonymous**: authentication IDs, session IDs, MXIDs, room IDs, and event IDs remain personal/linkable operational data and require the operator/auditor-only access and 90-day deletion controls the live component adds.

## Version pinning

The projectors are pinned to the ESS 26.6.2 reference: Synapse `v1.155.0` `events` and MAS `v1.19.0` `user_session_authentications`. The source-column allowlists in `base/records.py` are the schema fingerprint; an upgrade that changes them fails the offline contract until the fixtures are re-proved against the new migration set, per ADR 0018.
