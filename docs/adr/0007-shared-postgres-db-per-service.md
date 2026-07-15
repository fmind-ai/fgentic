---
type: Architecture Decision Record
title: Shared CloudNativePG, Database-per-Service
description: One shared CNPG cluster; every service gets its own database and purpose-scoped roles.
---

# 0007 — Shared CloudNativePG, Database-per-Service

Status: Accepted

## Context

The core stateful tenants need durable Postgres: **Synapse** (homeserver), **MAS** (auth), the **bridge** (the mautrix StateStore, so ghost registrations survive restarts — [ADR 0005](0005-matrix-a2a-bridge-appservice.md)), **kagent**, the optional **Keycloak** reference IdP, and the composed **knowledge store**. Every enabled external-network bridge adds another stateful tenant. The project principle is "**bridging infra, not embedded databases**": shared infrastructure exposed to independent apps without coupling them.

Alternatives considered and rejected:

1. **Bundled/embedded database per component** (e.g. ESS's in-chart Postgres, a sidecar DB per app). Simplest to bootstrap, but multiplies operators, backup policies, and connection configs — and contradicts the shared-infra principle by making each app own its own DB lifecycle.
1. **One shared database with a schema per service.** Lighter, but Synapse's **`C` collation** requirement is database-scoped, not schema-scoped, and mixing tenants in one DB muddies backup/restore and role isolation.
1. **A separate managed cloud DB (e.g. Cloud SQL).** Breaks provider independence — a hard non-goal for this platform.

## Decision

Run **one shared CloudNativePG cluster** (`platform-pg`, ns `postgres`) that exposes a **dedicated database + purpose-scoped role set per tenant**:

1. **`synapse`** — created with **`C` collation** (Synapse's hard requirement).
1. **`mas`** — the Matrix Authentication Service store.
1. **`bridge`** — the mautrix StateStore.
1. **`kagent`** — agent and session persistence.
1. **`keycloak`** — the optional reference IdP's realm, client, user, and session state.
1. **`knowledge`** — Fgentic's owned chunk/ACL schema, composed from pgvector in the same PostgreSQL operand. `knowledge_owner` owns migrations and ingestion objects; `knowledge_retrieval` is the separately credentialed read-only consumer. The exact schema and ranking boundary are in [Sovereign Grounding Store](../grounding.md).

Opt-in Kustomize components append one role/database without replacing the canonical Postgres layer. The shipped references use **`slackbridge`** and **`telegrambridge`**; their password Secrets do not exist in a normal core bootstrap.

TLS and the database/role pairing are enforced at connection admission. `pg_hba` contains one `hostssl <database> <role> … scram-sha-256` rule per admitted role, then rejects every other TLS or plaintext connection. Optional network components prepend only their own pair and remove it during the `NOLOGIN` offboard phase. This compensates for PostgreSQL's default `CONNECT` grant to `PUBLIC` instead of relying on schema ownership after a cross-database login has already succeeded. A single operator owns backups, connection policy, and failover for all stateful tenants; role passwords are SOPS-encrypted secrets decrypted in-cluster by Flux.

## Consequences

1. One operator, one backup policy, one connection posture — the shared-infra pattern demonstrated concretely.
1. `C` collation is set per-database, so Synapse's requirement is satisfied without forcing it on `mas`/`bridge`.
1. Scoped roles plus exact HBA pairs reject cross-database logins, so a compromised `bridge` credential cannot connect to Synapse's database or read its tables through the supported network path.
1. **Interop bridges each have their own database** — mautrix requires unrelated programs to avoid sharing a database, so Slack and Telegram compose `slackbridge` and `telegrambridge` into this cluster rather than sharing `bridge` or each other.
1. The knowledge store adds a database, extension, roles, schema, and indexes to the existing operand—not another database operator, StatefulSet, or bundled RAG platform. Empty catalog objects consume no steady-state connection, CPU, or memory.
1. The pattern mirrors the sibling `dev.fmind` bridging-infra principle, adapted from schema-per-service to database-per-service precisely because of Synapse's collation constraint.
