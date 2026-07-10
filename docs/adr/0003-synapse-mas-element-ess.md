# 0003 — Synapse + MAS + Element via Element Server Suite (ESS) Community

Status: Accepted

## Context

Matrix ([ADR 0002](0002-matrix-collaboration-fabric.md)) needs a concrete homeserver, an auth service, and clients. The platform exercises the **Application Service (appservice) API** hard ([ADR 0005](0005-matrix-a2a-bridge-appservice.md)) and wants to support **Element X** (the modern mobile client), which hard-requires OIDC + native sliding sync. Because this project is an _enterprise-credible reference_ and — unlike the sibling `dev.fmind` — **billing is not a constraint here**, the reference-grade option is chosen at this fork rather than the budget one.

Alternatives considered and rejected:

1. **Continuwuity / Tuwunel (Rust Conduit-family, ~256 MiB).** The correct pick under a tight RAM/cost budget (that is `dev.fmind`'s world), but a weaker Element X / MAS story and 2026 appservice-E2EE edge cases make it less convincing as an enterprise reference. Recorded here as the documented "budget swap."
1. **Dendrite (Go).** Attractive stack, but in maintenance-mode with an Application Service API upstream itself calls "not well tested" — the wrong foundation for a bridge showcase.
1. **Raw Synapse Helm, self-assembled with a separate MAS + Element.** Works, but re-implements the integration ESS already ships and maintains.

## Decision

Deploy the Matrix layer with **Element Server Suite (ESS) Community** (`element-hq/ess-helm`, the `matrix-stack` chart), which bundles:

1. **Synapse** — the reference homeserver, with the most complete Application Service API and richest bridge ecosystem (the exact surfaces this project exercises).
1. **Matrix Authentication Service (MAS)** — OIDC/OAuth2 (MSC3861), unlocking Element X and enterprise SSO/IdP integration.
1. **Element Web** — the always-available browser client (needs neither MAS nor sliding sync); **Element X** for mobile (needs both, which ESS supplies via native Simplified Sliding Sync, MSC4186).
1. **well-known delegation** for `fgentic.fmind.ai`.

ESS is _Element's own_ self-host distribution — the most credible "this is how a company would actually run it" reference. It is wired to the **shared CloudNativePG** cluster ([ADR 0007](0007-shared-postgres-db-per-service.md)), demonstrating external Postgres over a bundled DB.

## Consequences

1. Synapse's database must be created with **`C` collation** (a hard Synapse requirement) — provisioned explicitly on the shared cluster ([ADR 0007](0007-shared-postgres-db-per-service.md)).
1. Element X mobile works end-to-end (MAS + sliding sync), with Element Web as the zero-dependency fallback needing neither.
1. Inherited operational weight: Synapse's upgrade cadence + out-of-band security releases, a media store, and key backups — mitigated by ESS packaging + Flux image automation + a documented upgrade runbook; federation stays disabled unless needed, to shrink the surface.
1. Appservice registration is partly imperative (the homeserver loads `registration.yaml`; adding namespaces needs a restart) — git-declared as a Secret and handled in the bootstrap runbook.
1. The budget swap (Continuwuity/Tuwunel) is documented, so a cost-constrained fork is a known, bounded change rather than a re-architecture.
