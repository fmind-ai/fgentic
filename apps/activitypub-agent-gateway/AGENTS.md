# AGENTS.md — activitypub-agent-gateway

App-level layout and conventions for coding agents. Root platform context is in [.agents/AGENTS.md](../../.agents/AGENTS.md); the design is [ADR 0014](../../docs/adr/0014-activitypub-second-federation-transport.md) + [docs/fediverse.md](../../docs/fediverse.md).

## What this app is

A **self-contained** ActivityPub ↔ A2A gateway: it presents each platform agent as an AP `Service` actor and delegates fediverse mentions to kagent over A2A **through agentgateway**. It is the AP twin of `apps/matrix-a2a-bridge`, and is deliberately a separate app so the mautrix bridge stays AGPL-free and homeserver-portable and no agent holds a model credential.

This is the second federation transport ([standing rule](../../docs/fediverse.md)): additive to Matrix federation, never a replacement.

## Layout

- `cmd/gateway/main.go` — wires config → registry → a2a client → gateway; runs a public AP HTTP server and a private `/metrics` server; graceful shutdown.
- `internal/config` — `Config`, env-parsed via `caarlos0/env`, validated up front (fail fast).
- `internal/a2a` — thin `a2a-go` wrapper. **Local kagent targets only**; the asserted AP actor is forwarded as `X-User-Id`, the workload credential as a separate bearer. Remote/pinned A2A + Signed AgentCard trust is a different boundary landed elsewhere.
- `internal/httpsig` — inbound HTTP Message Signature verification (Cavage draft + RFC 9421) using only stdlib crypto (RSA PKCS1v15, RSA-PSS, Ed25519); body-digest binding + replay window; `HTTPKeyResolver` fetches the signer's key.
- `internal/policy` — the strict, fail-closed federation allowlist (`policy.json`) with a hot-reload `Store` (poll + atomic swap; invalid/unreadable ⇒ deny all).
- `internal/integrity` — FEP-8b32 object integrity proofs (`eddsa-jcs-2022`: Ed25519 over RFC 8785 JCS). `Sign`/`Verify` on the document, a `Signer` (SOPS PKCS#8 key, per-actor `verificationMethod`, `assertionMethod` Multikey), and an inbound `Verifier` + `HTTPKeyResolver`. Interop with **apsig** is pinned byte-for-byte by a golden vector (`mise run interop` re-derives it live).
- `internal/apgateway` — the AP surface: `Registry` (agents.yaml loader), Service `actor` (publishes its Multikey when signing is on), `webfinger` JRD, `store` (in-memory outbox of signed bytes + id index), `border` (signature + actor-key binding + allowlist + optional object-integrity), `gateway` (routes + inbox→border→A2A→outbox, signs replies, dereferences `/activities/{seq}`), `metrics` (aggregate governance counters, never model tokens).
- `chart/` — Helm chart (ClusterIP by default; the single exact public `HTTPRoute` is **gated off**; optional policy + signing-key mounts). `component/` — namespace-neutral Kustomize Component projecting the mutable `policy.json` ConfigMap. `deploy/` — Namespace + HelmRelease + Component Flux unit; **opt-in**, not yet in the reconciled DAG.

## Conventions (match the bridge)

1. Go, type-safe, small composable units; errors wrapped with `%w`; no ignored `err`; no tech debt.
1. Inbound AP content is **untrusted**. Only delegate when the note actually mentions the routed agent, so a stray/relayed delivery cannot spend an LLM invocation. Real sender authorization is the signed border (issue #211).
1. `contextId` is derived from `(ghost, actor, thread)` and **never reused across agents**.
1. Reach kagent only via `a2a-go` through agentgateway — never a hand-rolled JSON-RPC client, never a model credential in this app.
1. Public exposure is governance-gated: keep `httpRoute.enabled=false` until the AP federation border is in force.
1. Every dependency stays permissive (MIT / Apache-2.0); keep `NOTICE` current; never add AGPL.
1. Object integrity is bidirectional and fail-closed: outbound replies always carry a proof when a key is mounted; inbound proofs are mandatory only when `integrity.requireInbound` is set (needs the policy border). Never weaken the tamper-rejection or actor-controller binding.
1. Validation gates: `mise run check` + `mise run test` warning-free. Coverage ratchets live in `scripts/check-coverage.sh` (`internal/apgateway`, `internal/a2a`, `internal/config`, `internal/httpsig`, `internal/policy`, `internal/integrity`).
