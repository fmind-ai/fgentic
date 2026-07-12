# activitypub-agent-gateway

A self-contained Go service that exposes each Fgentic platform agent as an **ActivityPub `Service` actor**, so a Fediverse user (Mastodon, GoToSocial, …) can follow it by its handle, `@mention` it, and receive a governed reply — backed by an A2A delegation to kagent through agentgateway.

It is the first surface of **ActivityPub as a second, additive federation transport** ([ADR 0014](../../docs/adr/0014-activitypub-second-federation-transport.md), [fediverse spec](../../docs/fediverse.md)). It is deliberately **not** part of the mautrix bridge, so that bridge stays AGPL-free and homeserver-portable, and — like every other caller — this gateway reaches kagent only through agentgateway, so **no agent holds a model credential**.

## What it does today (M18 F1)

- Serves each `agent-<name>` as an AP `Service` actor at `/ap/agents/<name>`, with `/.well-known/webfinger` resolving `acct:agent-<name>@<server_name>`.
- Turns an inbound `Create(Note)` mention into one A2A `message/send`, threaded by a per-`(actor, thread)` `contextId` that is never reused across agents.
- Publishes the reply as a `Create(Note)` `inReplyTo` the triggering object, in the agent's outbox.

Inbound AP content is **untrusted** (prompt injection is threat #1). This app lands only the actor surface: the HTTP-Signature/allowlist border, object integrity, per-actor budget admission, and honest bot/attribution audit ([fediverse spec §3](../../docs/fediverse.md)) gate real public exposure and land in later M18 issues. The public HTTPRoute is **disabled by default**.

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
