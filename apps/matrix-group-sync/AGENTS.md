# AGENTS.md — matrix-group-sync

App-level layout and conventions for coding agents. Root platform context is in [../../AGENTS.md](../../AGENTS.md); the design is [ADR 0009](../../docs/adr/0009-agent-authorization-model.md) + [docs/group-sync.md](../../docs/group-sync.md).

## What this app is

A **self-contained**, **security-sensitive** GitOps reconciler that materializes authoritative IdP-group membership (Keycloak) into managed Matrix room membership through a **narrowly scoped access-manager Matrix identity**. IdP groups declare desired membership; this controller converges it into room state; the bridge authorizes only within that already-materialized state. It is deliberately a separate app from `apps/matrix-a2a-bridge`.

## Layout

- `cmd/matrix-group-sync/main.go` — wires config → bindings → Keycloak directory + Matrix room manager → the 60s reconcile loop; serves `/metrics`, `/healthz`, `/readyz` on an internal port (no public surface).
- `internal/config` — `Config`, env-parsed via `caarlos0/env`, validated up front (fail fast). Secret file PATHS only; the token and client secret are read from mounted files, never the process env.
- `internal/bindings` — the git-declared `group -> room` bindings parser: absolute group paths, full server-qualified aliases, unique group/room, agent-prefix checks; unknown fields rejected.
- `internal/mxid` — the ONE place a principal is turned into a full MXID and checked against the local server. Every federation-safety decision goes through `Format`/`IsLocal`/`IsLocalGhost`. Never match by localpart alone.
- `internal/directory` — `Directory` interface + types (`Member{Sub,Localpart}`, `Snapshot{Groups,Complete}`) and the Keycloak client-credentials READ client. `Complete=false` on ANY partial/failed paginated read is load-bearing.
- `internal/matrix` — `RoomManager` interface (resolve alias, room state, account exists, invite, kick, ban) + the mautrix/go adapter. NORMAL client APIs only — no Synapse-admin, no MAS admin, no Spaces, no impersonation.
- `internal/reconcile` — the security core. Fail-closed in every ambiguous direction; audit-only by default.
- `internal/metrics` — governance counters + `reconcile_stalled` and `revocation_slo_breach` alert gauges.
- `chart/` — Helm chart (ClusterIP metrics only, default-deny NetworkPolicy). `deploy/` — Namespace + HelmRelease + quota Flux unit; **opt-in**, not in the reconciled DAG.

## Fail-closed decision points (do not weaken)

1. Partial/failed directory read or timeout ⇒ NO grants and NO bulk removals; retain last-known Matrix state; alert after two missed intervals.
1. Ambiguous mapping (duplicate `sub`, duplicate `matrix_localpart`, or a member with no `sub`) ⇒ whole-cycle abort of mutation (no grants, no removals).
1. Missing/invalid `matrix_localpart` for a member ⇒ that member is ungrantable (skip); never guess an identity.
1. Nonexistent Matrix account (profile lookup) or a failed lookup ⇒ NO invite.
1. Unmanaged room, unexpected creator (not the access-manager), non-v12 room, or power-level drift ⇒ grants blocked for that room.
1. Only LOCAL, non-ghost, non-access-manager members are ever revoked. A partner (remote) member is never evicted by a local IdP group.
1. Audit-only (default) makes zero Matrix mutations and raises no revocation-SLO alert.

## Conventions (match the bridge and AP gateway)

1. Go, type-safe, small composable units; errors wrapped with `%w`; no ignored `err`; deadlines on I/O; no tech debt.
1. Keycloak is untrusted external input parsed into a trusted `Snapshot` at the boundary. Never silently drop members — set `Complete=false`.
1. The access-manager identity is the ONLY Matrix mutator, and only in rooms it created. Never add a Synapse/MAS admin path or a Space selector.
1. Federation-safe: form the full MXID and check the homeserver; a local IdP group can only assert a local MXID.
1. Validation gates: `mise run check` + `mise run test` warning-free. Coverage ratchets live in `scripts/check-coverage.sh` (`internal/reconcile`, `internal/bindings`, `internal/mxid`, `internal/config`, `internal/directory`, `internal/metrics`). The `internal/matrix` mautrix adapter is exercised by the deferred live-cluster acceptance.
1. Every dependency stays permissive (MPL-2.0 / MIT / Apache-2.0); keep `NOTICE` current; never add AGPL.
