# matrix-group-sync

A self-contained Go reconciler that materializes authoritative **IdP-group membership** into **managed Matrix room membership**, so enterprise access (Keycloak groups) drives who is in which agent room — the authorization boundary the bridge then enforces on the message path ([ADR 0009](../../docs/adr/0009-agent-authorization-model.md), [docs/group-sync.md](../../docs/group-sync.md)).

It is a one-way GitOps controller: git declares exact group→room bindings, Keycloak is the authoritative membership source, and a **narrowly scoped access-manager Matrix identity** drives normal invites and kicks. It holds **no Synapse-admin credential, no MAS `urn:mas:admin` token, and uses no Spaces or appservice impersonation**.

## What it does

- Reads git-declared exact `Keycloak group path -> managed room` bindings, each with an explicit agent set.
- Reads authoritative group membership plus the administrator-managed `matrix_localpart` attribute from Keycloak, **keys reconciliation by the stable upstream `sub`**, forms the **full local MXID** `@<matrix_localpart>:<server_name>`, and **fails closed** on duplicates, missing/invalid localparts, or nonexistent Matrix accounts.
- Reconciles a complete paginated directory snapshot every 60s. A **partial read, timeout, or ambiguous mapping creates no grants and no bulk removals**, retains last-known Matrix state, and alerts after two missed intervals; ordinary revocation is bounded to a two-minute SLO.
- Invites on grant, withdraws a pending invite or kicks on revoke, keeps humans at power level 0, and applies **local IdP groups only to local MXIDs** (federation-safe — never asserts a partner-group membership).

## Audit-only first

`config.enforce=false` is the default: the reconciler computes and reports every membership diff and makes **zero** Matrix mutations. It fails closed for grants on unmanaged rooms, unexpected creators, or power-level drift. Real additions, removals, and the revocation-SLO alert enable together only when `enforce=true`, after reviewed room adoption.

## Layout

- `cmd/matrix-group-sync` — entrypoint: config → bindings → Keycloak + Matrix clients → 60s reconcile loop + internal metrics/health server.
- `internal/config` — typed, env-parsed, fail-fast configuration.
- `internal/bindings` — the git-declared group→room bindings parser (fail-closed validation).
- `internal/mxid` — full-MXID formation and the single federation-safety chokepoint.
- `internal/directory` — the IdP membership source: `Directory` interface + the Keycloak client-credentials read client (paginated, `Complete` fail-closed).
- `internal/matrix` — the Matrix room-management boundary: `RoomManager` interface + the mautrix/go access-manager adapter (normal client APIs only).
- `internal/reconcile` — the security core: the fail-closed diff-and-converge logic.
- `internal/metrics` — governance counters + the stall and revocation-SLO alert gauges.
- `chart/` — the Helm chart; `deploy/` — its Flux unit (Namespace + HelmRelease + quota), **opt-in**, not in the reconciled DAG.

## Development

```sh
mise run format   # goimports + gofumpt + dprint
mise run check    # golangci-lint + govulncheck + dprint + gitleaks
mise run test     # race + per-package coverage ratchets
mise run build    # static distroless-nonroot binary
```

The reconciler's decision logic is fully proven offline against fakes (fake Keycloak directory + fake Matrix client); the live-cluster invite/revocation/IdP-outage flow is the deferred acceptance path a runtime owner runs against Keycloak + Matrix. All dependencies are permissive — see `NOTICE`.
