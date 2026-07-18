# activitypub-agent-gateway

A self-contained Go service that exposes each Fgentic platform agent as an **ActivityPub `Service` actor**, so a Fediverse user (Mastodon, GoToSocial, …) can follow it by its handle, `@mention` it, and receive a governed reply — backed by an A2A delegation to kagent through agentgateway.

It is the first surface of **ActivityPub as a second, additive federation transport** ([ADR 0014](../../docs/adr/0014-activitypub-second-federation-transport.md), [fediverse spec](../../docs/fediverse.md)). It is deliberately **not** part of the mautrix bridge, so that bridge stays AGPL-free and homeserver-portable, and — like every other caller — this gateway reaches kagent only through agentgateway, so **no agent holds a model credential**.

## What it does today (M18 F1)

- Serves each `agent-<name>` as an AP `Service` actor at `/ap/agents/<name>`, with `/.well-known/webfinger` resolving `acct:agent-<name>@<server_name>`.
- Turns an inbound `Create(Note)` mention into one A2A `SendMessage`, threaded by a per-`(actor, thread)` `contextId` that is never reused across agents.
- Publishes the reply as a `Create(Note)` `inReplyTo` the triggering object, in the agent's outbox.

## Federation policy border (M18 F3)

Inbound AP content is **untrusted** (prompt injection is threat #1). The border enforces, before any A2A call:

- **HTTP Message Signature** verification (RFC 9421 + Cavage fallback), stdlib crypto only, with body-digest binding and a replay window.
- **Actor-key binding**: a valid signature from key K only authorizes activities whose actor is K's owner.
- A strict, **fail-closed allowlist** (`policy.json`: signing domains + exact actor URIs) that **hot-reloads from git** without a pod restart — a parse error, unreadable file, or empty allowlist denies everything.

An unsigned, off-allowlist, or mis-bound inbound is dropped with content-free evidence and **zero** A2A calls. Object integrity, per-actor budget admission, and bot/attribution audit ([fediverse spec §3](../../docs/fediverse.md)) land in later M18 issues; the public HTTPRoute stays **disabled by default** until the border is proven in force.

## Outbound signature negotiation

Outbound inbox delivery prefers **RFC 9421**, signing `@method`, `@target-uri`, and the RFC 9530 `Content-Digest` with a `created` timestamp. A synchronous `401` triggers one Cavage retry; the successful profile is remembered per remote authority in a bounded, process-local cache. Network errors and 5xx responses are never retried with the other profile because the remote may already have processed the activity.

Both profiles use a dedicated RSA PKCS#1 v1.5/SHA-256 transport key, the Mastodon interoperability baseline, published as each delivering actor's `#main-key`. The separate Ed25519 key continues to sign FEP-8b32 object proofs, so FEP-8b32/844e/c390 object-layer identities remain independent of this hop-by-hop negotiation.

When Group or status-feed delivery is enabled, set `HTTP_SIGNATURE_KEY_PATH` to a PKCS#8 or PKCS#1 RSA private key of at least 2048 bits. The Helm chart mounts `httpSignature.secretKey` (`rsa.pem`) from the SOPS-backed signing Secret alongside—but never in place of—the Ed25519 integrity key.

## Layout

- `cmd/gateway` — entrypoint (two HTTP servers: public AP + private metrics).
- `internal/config` — typed, env-parsed, fail-fast configuration.
- `internal/a2a` — thin wrapper over the official `a2a-go` client (local kagent targets only).
- `internal/apgateway` — the AP surface: agent registry, Service actor, WebFinger, inbox→A2A→outbox.
- `chart/` — the Helm chart; `deploy/` — its Flux unit (Namespace + HelmRelease), opt-in.

## Development

```sh
mise run format   # goimports + gofumpt + dprint
mise run check    # golangci-lint + govulncheck + dprint + gitleaks
mise run test     # race + per-package coverage ratchets
mise run build    # static distroless-nonroot binary
```

All dependencies are permissive (MIT / Apache-2.0) — see `NOTICE`. Never add an AGPL dependency.
