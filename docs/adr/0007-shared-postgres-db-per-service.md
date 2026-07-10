# 0007 — Shared CloudNativePG, Database-per-Service

Status: Accepted

## Context

Three stateful tenants need durable Postgres: **Synapse** (homeserver), **MAS** (auth), and the **bridge** (the mautrix StateStore, so ghost registrations survive restarts — [ADR 0005](0005-matrix-a2a-bridge-appservice.md)). The project principle is "**bridging infra, not embedded databases**": shared infrastructure exposed to independent apps without coupling them.

Alternatives considered and rejected:

1. **Bundled/embedded database per component** (e.g. ESS's in-chart Postgres, a sidecar DB per app). Simplest to bootstrap, but multiplies operators, backup policies, and connection configs — and contradicts the shared-infra principle by making each app own its own DB lifecycle.
1. **One shared database with a schema per service.** Lighter, but Synapse's **`C` collation** requirement is database-scoped, not schema-scoped, and mixing tenants in one DB muddies backup/restore and role isolation.
1. **A separate managed cloud DB (e.g. Cloud SQL).** Breaks provider independence — a hard non-goal for this platform.

## Decision

Run **one shared CloudNativePG cluster** (`platform-pg`, ns `postgres`) that exposes a **dedicated database + scoped role per tenant**:

1. **`synapse`** — created with **`C` collation** (Synapse's hard requirement).
1. **`mas`** — the Matrix Authentication Service store.
1. **`bridge`** — the mautrix StateStore.

TLS is enforced end-to-end (`sslmode=require`). A single operator owns backups, connection policy, and failover for all stateful tenants; role passwords are SOPS-encrypted secrets decrypted in-cluster by Flux.

## Consequences

1. One operator, one backup policy, one connection posture — the shared-infra pattern demonstrated concretely.
1. `C` collation is set per-database, so Synapse's requirement is satisfied without forcing it on `mas`/`bridge`.
1. Scoped roles keep tenants isolated (no cross-database access), so a compromised `bridge` role cannot read Synapse's tables.
1. **Future interop bridges each want their own database** — mautrix officially requires a dedicated DB per bridge, so the external-network phase adds one CloudNativePG database per bridge (`telegram`, `slack`, …) rather than sharing `bridge`.
1. The pattern mirrors the sibling `dev.fmind` bridging-infra principle, adapted from schema-per-service to database-per-service precisely because of Synapse's collation constraint.
