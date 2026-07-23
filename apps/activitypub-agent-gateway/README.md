# activitypub-agent-gateway

A self-contained Go service that exposes each Fgentic platform agent as an **ActivityPub `Service` actor**, so a Fediverse user (Mastodon, GoToSocial, …) can follow it by its handle, `@mention` it, and receive a governed reply — backed by an A2A delegation to kagent through agentgateway.

It is the first surface of **ActivityPub as a second, additive federation transport** ([ADR 0014](../../docs/adr/0014-activitypub-second-federation-transport.md), [fediverse spec](../../docs/fediverse.md)). It is deliberately **not** part of the mautrix bridge, so that bridge stays AGPL-free and homeserver-portable, and — like every other caller — this gateway reaches kagent only through agentgateway, so **no agent holds a model credential**.

## What it does today (M18 F1)

- Serves each `agent-<name>` as an AP `Service` actor at `/ap/agents/<name>`, with `/.well-known/webfinger` resolving `acct:agent-<name>@<server_name>`.
- Durably accepts each identified inbound `Create(Note)` once, then runs one bounded A2A `SendMessage` asynchronously, threaded by a per-`(actor, thread)` `contextId` that is never reused across agents.
- Publishes the reply as a `Create(Note)` `inReplyTo` the triggering object, in the agent's outbox.

## Federation policy border (M18 F3)

Inbound AP content is **untrusted** (prompt injection is threat #1). The border enforces, before any A2A call:

- **HTTP Message Signature** verification (RFC 9421 + Cavage fallback), stdlib crypto only, with body-digest binding, mandatory request-target coverage, and a covered timestamp inside the bounded replay window.
- **Actor-key binding**: a valid signature from key K only authorizes activities whose actor is K's owner.
- A strict, **fail-closed allowlist** (`policy.json`: signing domains + exact actor URIs) that **hot-reloads from git** without a pod restart — a parse error, unreadable file, or empty allowlist denies everything.

An unsigned, off-allowlist, or mis-bound inbound is dropped with content-free evidence and **zero** A2A calls. Object integrity, per-actor budget admission, and bot/attribution audit ([fediverse spec §3](../../docs/fediverse.md)) land in later M18 issues; the public HTTPRoute stays **disabled by default** until the border is proven in force.

RFC 9421 signatures must cover `@method` plus either `@target-uri` or both `@path` and `@authority`; Cavage signatures must cover `(request-target)` and `host`. Both profiles must also cover a `created` parameter or `Date` header. `SIGNATURE_MAX_SKEW` bounds how old a request may be (12 hours by default), while `SIGNATURE_FUTURE_SKEW` tolerates at most five minutes of signer clock lead. Missing, stale, future-dated, or unbound signatures fail before key resolution or A2A delegation.

## Durable asynchronous inbox

Every delegating `Create` must carry an absolute HTTPS activity `id`. After signature, actor, allowlist, and object-integrity verification, the gateway atomically inserts that ID plus its bounded body into Postgres and returns `202 Accepted`; the A2A call and any group fan-out run off the request goroutine. `Location` is an opaque, unguessable status capability: it returns `202` while work is pending/running, a content-free terminal state for no-reply outcomes, or the exact persisted reply Activity after success. An exact retry returns the same status location without repeating any budget reservation or A2A call, including after restart. Reusing an ID with a different actor, route, target, or body returns `409 Conflict`.

The single processor changes `pending` to `running` before any budget or A2A side effect. A process restart resumes pending work, preserves terminal outcomes and successful reply bytes, and keeps the reply's canonical Activity IRI dereferenceable from Postgres even though the collection cache is process-local. It marks interrupted running work failed without replay: an unknown prior A2A attempt is never repeated merely to obtain a reply. Raw inbox bodies are erased as soon as a row becomes terminal; a SHA-256 digest retains exact-retry collision detection. Terminal outcomes are pruned periodically after `ACTIVITY_RETENTION` (seven days by default), even when traffic stops.

`ACTIVITY_QUEUE_CAPACITY` (32 by default) atomically caps all pending and running bodies, limiting the default worst-case retained input to 32 MiB. A new unique ID receives `503 Service Unavailable` plus `Retry-After` while full and consumes no budget; an already-recorded duplicate still resolves to its cached status. A valid non-mention is inserted directly as terminal `ignored`, never exposed to the worker and never charged.

The chart reads `DATABASE_URL` from `database.secretName` / `database.key` (`activitypub-agent-gateway-db` / `url` by default). Disabling the database is local-only and requires the federation policy border to be off; any policy-gated deployment fails fast without durable dedup, and enabling the public HTTPRoute fails unless that signed policy border is enabled. The URL belongs in a SOPS-encrypted cluster Secret and must point to the gateway's own scoped Postgres database and role. `mise run test:postgres` runs the restart, collision, atomic-ignore, and concurrent-capacity contract against a disposable digest-pinned Postgres.

## Outbound signature negotiation

Outbound inbox delivery prefers **RFC 9421**, signing `@method`, `@target-uri`, and the RFC 9530 `Content-Digest` with a `created` timestamp. A synchronous `401` triggers one Cavage retry; the successful profile is remembered per remote authority in a bounded, process-local cache. Network errors and 5xx responses are never retried with the other profile because the remote may already have processed the activity.

Both profiles use a dedicated RSA PKCS#1 v1.5/SHA-256 transport key, the Mastodon interoperability baseline, published as each delivering actor's `#main-key`. The separate Ed25519 key continues to sign FEP-8b32 object proofs, so FEP-8b32/844e/c390 object-layer identities remain independent of this hop-by-hop negotiation.

When Group or status-feed delivery is enabled, set `HTTP_SIGNATURE_KEY_PATH` to a PKCS#8 or PKCS#1 RSA private key of at least 2048 bits. The Helm chart mounts `httpSignature.secretKey` (`rsa.pem`) from the SOPS-backed signing Secret alongside—but never in place of—the Ed25519 integrity key.

The gateway remains opt-in until issue #489 composes it into the demo profile, so operators provision this key manually with the other ActivityPub keys. Copy `infra/secrets/activitypub-agent-gateway-signing-key.sops.yaml.example` to the target cluster's secret set, generate a distinct transport key with `openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out rsa.pem`, add its PEM as `stringData.rsa.pem`, then encrypt the manifest in place with SOPS before committing it. Never replace `stringData.ed25519.pem`: the two keys have separate identities and rotation lifecycles. The opt-in HelmRelease deliberately fails fast if `rsa.pem` is absent rather than silently sending an Ed25519 transport signature that Mastodon cannot discover.

## Private Matrix-to-Fediverse broker

`FEDIVERSE_BROKER_TOKEN` enables two authenticated routes on the internal metrics listener: `/internal/v1/fediverse/resolve` and `/internal/v1/fediverse/delegate`. They are not mounted on the public listener and the chart's HTTPRoute cannot select them. The broker resolves a strict `acct:` handle through SSRF-guarded WebFinger and its exact actor link. An advertised FEP-844e `implements` entry returns the exact A2A endpoint and card URL to the bridge, which performs its existing pinned Signed AgentCard verification. Without A2A, the broker requires the actor document to carry a fresh FEP-8b32 proof under the mapping's exact actor, verification-method, Multikey, and maximum-age pins before sending a signed `Create(Note)`.

The fallback is intentionally asynchronous: a successful call acknowledges signed inbox delivery, not a synchronous agent answer. Its object is signed by the Ed25519 integrity key and its HTTP request by the RSA delivery key. The local `A2A_API_KEY` is never accepted by this client path. Helm `fediverseBroker.enabled` exposes only the side port on the ClusterIP Service, admits only `fediverseBroker.fromNamespaces`, and reads the shared bearer from a SOPS-backed Secret.

## Layout

- `cmd/gateway` — entrypoint (two HTTP servers: public AP + private metrics).
- `internal/config` — typed, env-parsed, fail-fast configuration.
- `internal/a2a` — thin wrapper over the official `a2a-go` client (local kagent targets only).
- `internal/activitystate` — Postgres-backed unique activity ledger and asynchronous work queue.
- `internal/apgateway` — the AP surface plus the private pinned-handle resolver and signed fallback broker.
- `chart/` — the Helm chart; `deploy/` — its Flux unit (Namespace + HelmRelease), opt-in.

## Development

```sh
mise run format   # goimports + gofumpt + dprint
mise run check    # golangci-lint + govulncheck + dprint + gitleaks
mise run test     # race + per-package coverage ratchets
mise run build    # static distroless-nonroot binary
```

All dependencies are permissive (MIT / Apache-2.0) — see `NOTICE`. Never add an AGPL dependency.
