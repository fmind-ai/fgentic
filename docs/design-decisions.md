---
type: Decision Register
title: Design Decisions D1–D18
description: The durable register of settled design decisions with the evidence behind each; revisit via a new ADR, never a drive-by PR (§4).
---

# Design Decisions D1–D18 (formerly SPEC §4)

> The durable record of _why_ the system looks the way it does. Revisit via a new ADR, never a drive-by PR. Section references `§N` map per the table in [.agents/AGENTS.md](../.agents/AGENTS.md).

Each entry records the problem found in the first draft, the evidence, and what the code/manifests now do. They are kept as the durable record of _why_ the system looks the way it does.

### D1 — Apps depend on infra via Flux Kustomizations (was: broken `dependsOn`)

The draft's bridge **HelmRelease** listed Flux **Kustomizations** in `dependsOn` — but `HelmRelease.spec.dependsOn` only resolves other HelmReleases; the release would wait forever. **Now:** each app is wrapped in its own Flux `Kustomization` (`clusters/base/apps.yaml` → `apps/matrix-a2a-bridge/deploy/`) that `dependsOn` the infra Kustomizations — the DAG stays homogeneous.

### D2 — The reserved ingress IP is pinned by name (was: orphaned)

Terraform reserved a static address the Traefik Service never used; DNS would break on Service recreation. **Now:** the Traefik values carry `networking.gke.io/load-balancer-ip-addresses: fgentic-ingress-ip` (GKE resolves the reserved address by name; harmless on other providers/k3d).

### D3 — Bounded, per-room-ordered dispatch (was: unbounded and unordered)

The appservice API delivers events on a single linearised transaction queue (Synapse #17621 documents the stall mode). mautrix's `EventProcessor` defaults to `AsyncHandlers` (verified in source: one goroutine per event), so a synchronous A2A call in the handler doesn't block the queue — but it means **unbounded concurrent LLM calls with no per-room ordering**. **Now:** `HandleMessage` only classifies and enqueues; a dispatcher drains per-room FIFO queues under a global concurrency cap (`CONCURRENCY`, default 16) and accepted running-plus-queued caps (32 per room, 256 globally). Helm rejects more than one replica and uses a zero-surge rollout (`maxSurge: 0`, `maxUnavailable: 1`), because even a rollout overlap would split a room across independent dispatchers. Shutdown first closes appservice intake and drains its synchronous processor, then gives accepted delegations 25 seconds before cancellation under a 45-second pod grace. Any target rejected or dropped at that boundary emits a terminal `shutdown` audit rather than disappearing after dedup. The registration sets `rate_limited: false` so ghost replies bypass Synapse's `rc_message` (0.2 msg/s default) — the bridge enforces its own budgets (D7).

**Revised by D17 ([ADR 0016](adr/0016-durable-delegation-ledger.md)):** the ordering and active-concurrency goals remain, but initial mentions now enter the Postgres ledger before HTTP 200. Database claims enforce cross-restart room FIFO. The same 32-per-room/256-global defaults now bound every non-terminal durable job, including delayed and leased work, through serialized admission rather than process-local queue length.

### D4 — Postgres-backed bridge state (was: all in-memory)

Pod restarts lost conversation threading, ghost registration state, and the transaction dedup cache — a redelivered transaction **re-invoked agents** (duplicate LLM spend + duplicate replies). **Now:** the `bridge` CNPG database backs the mautrix SQL StateStore and the bridge's own tables (§5); `DATABASE_URL` empty falls back to in-memory for local dev only.

**Extended by D17 ([ADR 0016](adr/0016-durable-delegation-ledger.md)):** Postgres now also binds each appservice transaction ID to its exact body hash and stores one checked, fenced job per `(Matrix event, target ghost)` before acknowledgement. The legacy processed-event table remains only as a minimum 24-hour upgrade tombstone.

### D5 — Context threads keyed by `(room, agent)` (was: per room)

kagent maps `contextId` 1:1 to a persistent session (verified: `SessionID = task.ContextID`); a room-keyed contextId would collide sessions across two agents in one room and cross-contaminate their memory. **Now:** `bridge_contexts` is keyed `(room_id, ghost)`.

**Narrowed by D18 ([ADR 0017](adr/0017-permission-aware-identity-binding.md)):** a retrieval-capable Agent never reuses that conversational context. Each initial retrieval delegation omits `contextId` and receives a fresh server-generated session, preventing previously grounded chunks from bypassing a later audience, ACL, or classification decision.

### D6 — Federation-safe target resolution + sender policy (was: localpart spoofing hole)

The draft matched mentions by localpart only — in a federated room, `@agent-k8s:evil.example` would have invoked the _local_ agent; and any room member could invoke any agent. **Now:** a mention resolves to an agent only when its homeserver is the bridge's own server; each agent in `agents.yaml` carries `allowedServers` (extra homeservers, own server always allowed — federated senders **deny-by-default**) and `allowedSenders` (anchored `*`-globs over full user IDs). Configured `bridgedOrigins` add a second fail-closed identity class for local MXIDs owned by external appservices: one anchored namespace maps to one bounded network label, overlaps fail startup, and the full MXID must explicitly match `allowedSenders`. This is the bridge-level half of the federation/interop trust model (§7–§8), unit-tested.

### D7 — Rate limits as LLM-spend guards (was: none)

Every mention is an LLM invocation; a chatty or malicious room drains the model budget (the closest prior-art project died of exactly this). **Now:** token buckets per `(sender, agent)` (default 6/min, burst 3) and per room (default 30/min, burst 10), rejecting with a polite ghost notice; bridged keys prefix the bounded network while retaining the complete MXID and agent, so two remote identities never share a budget. A separate response plane bounds all bridge-generated directory, denial, and rate-limit notices with the same sender/room defaults; it never consumes invocation capacity, and exhaustion emits no further Matrix event. Each of the four keyed maps has a hard 4096-bucket default. A new key fails closed at capacity; the bridge never evicts an active bucket and resets its burst, while idle cleanup scans at most once per minute. The dispatcher independently bounds accepted running plus queued work at 32 jobs per room and 256 globally. Overflow is silent and occurs before rate admission or A2A, with a content-free `queue_full` audit. Agentgateway token-unit rate limits on the LLM route are the second layer (§9). The cross-org listener adds a fail-closed global quota keyed by the verified JWT `azp`, using each required `SendMessage` `maxTokens` value as admission cost (§8.3). That value is a reservation, not actual usage; provider/model token metrics arise on the separate downstream hop and remain aggregate rather than per consumer.

**Revised by D17 ([ADR 0016](adr/0016-durable-delegation-ledger.md)):** the invocation/notice token buckets, bounded key maps, 32/256 capacity defaults, and stable `queue_full` outcome remain. Initial targets are now capacity-checked atomically against non-terminal ledger rows, room before global. A refusal commits as a content-scrubbed terminal `denied` row before HTTP 200 instead of disappearing from process memory; `CONCURRENCY` separately bounds active leases.

### D8 — Loop-safe reply semantics (was: loopable)

A federated agent or third-party bot could `@mention` a local agent whose reply mentions back — unbounded ping-pong. **Now:** ghost replies are **`m.notice`** (the Matrix convention for bot output) and the bridge never treats `m.notice` as a delegating message; the room-level rate bucket (D7) is the backstop.

### D9 — Long tasks via `tasks/get` + Matrix edits (was: hard 60s ceiling)

Real agent work exceeds 60s; kagent deliberately sets no timeout on its agent client and proxies `tasks/get`. **Now:** §6's model — non-terminal `Task` → placeholder reply → poll with backoff up to `TASK_TIMEOUT` (10m) → `m.replace`-edit the placeholder into the final answer. The whole-task clock begins at the first persisted A2A attempt boundary rather than ledger admission, so durable room backlog is excluded. Matrix edits are the deliberate open-standard substitute for streaming; `message/send` stays non-streaming.

### D10 — A2A wire-version pinned to kagent's (was: v2.0.0 vs v2.3.1 drift)

kagent selects the wire format by header, defaulting to legacy 0.3 JSON-RPC, with the default slated to flip around kagent 0.11. **Now:** the bridge pins the SDK version kagent itself runs (v2.3.1); the §12 contract test is the tripwire for the 0.11 flip.

### D11 — kagent treated as unauthenticated (documented, mitigated)

kagent v0.9.11's default `auth.mode: unsecure` derives identity from a spoofable `X-User-Id` header (fallback `admin@kagent.dev`) and its authorizer is a no-op — **NetworkPolicies are the only boundary in front of every agent**. **Now:** a NetworkPolicy on the `kagent` namespace admits only agentgateway, the bridge, kagent's own pods, and monitoring; the bridge stamps `X-User-Id` with the real Matrix sender so kagent sessions/audit attribute to humans; kagent's OIDC (`trusted-proxy` + oauth2-proxy) is tracked for adoption when it stabilizes; any future cross-org exposure goes through agentgateway JWT/mTLS (§8.3), never kagent directly.

**Review (2026-07-11): not adopted.** The newest non-prerelease upstream release is still [v0.9.11](https://github.com/kagent-dev/kagent/releases/tag/v0.9.11); [v0.10.0-beta6](https://github.com/kagent-dev/kagent/releases/tag/v0.10.0-beta6) is a SemVer prerelease and no v0.11 release exists. More importantly, both versions' `trusted-proxy` mode ([v0.9.11 source](https://github.com/kagent-dev/kagent/blob/v0.9.11/go/core/internal/httpserver/auth/proxy_authn.go#L28-L78), [beta6 source](https://github.com/kagent-dev/kagent/blob/v0.10.0-beta6/go/core/internal/httpserver/auth/proxy_authn.go#L28-L78)) decodes the JWT payload without validating its signature, relies on oauth2-proxy as the actual authentication boundary, and still installs the [no-op authorizer](https://github.com/kagent-dev/kagent/blob/v0.10.0-beta6/go/core/cmd/controller/main.go#L32-L55). That browser-oriented proxy flow cannot authenticate Fgentic's non-browser Matrix bridge: Matrix events carry a sender ID, not the sender's OIDC bearer token, so the bridge can only assert that sender through `X-User-Id`. Upstream's proposed non-browser hook in [kagent #1890](https://github.com/kagent-dev/kagent/issues/1890) is an optional kagent-side A2A external authorizer with allowlisted credential forwarding; gateway-only enforcement remains valid, but that issue does not itself sanitize a gateway projection. kagent's per-Agent/API authorization remains open in [#1270](https://github.com/kagent-dev/kagent/issues/1270). Re-evaluate only after #1270 (or its accepted successor) ships in a non-prerelease release **and** #1890's hook, or an accepted successor, is stable and configured end to end. Until both conditions hold, `X-User-Id` is attribution rather than authentication and the NetworkPolicy conformance guard remains mandatory.

D18 narrows that gateway-projection pattern to permission-aware retrieval without changing D11 globally: kagent still does not authenticate the end user, and the projected header remains trustworthy only on the exact workload-authenticated, admission-constrained, NetworkPolicy-guarded retrieval path.

### D12 — Data durability (was: zero backups)

A single PVC loss would have destroyed Synapse history, MAS identities, kagent sessions, and bridge state simultaneously. **Now:** CNPG WAL archiving + nightly `ScheduledBackup` to a Terraform-provisioned GCS bucket via a keyless Workload-Identity binding; `instances: 3` documented as the production profile. Still open: the Synapse **media store** decision (S3/GCS media provider vs snapshot-backed PVC) — tracked as issue #62 (M9).

### D13 — Supply chain: digest-pinned, signed images (was: mutable `latest`)

**Now:** `cd.yml` builds multi-arch → trivy-scans the pushed digest → emits Syft SBOM plus SLSA/SBOM attestations → cosign-signs the image and an OCI Helm chart (keyless OIDC) → commits both immutable digests into the deployment source. Flux keyless-verifies the chart's exact workflow identity before helm-controller can install it. CI (`ci.yml`) runs the same `mise` gates as the git hooks plus a clean-tree assertion. The one-time safe bootstrap and operator verification commands are in [docs/security/supply-chain.md](security/supply-chain.md).

### D14 — NetworkPolicies pre-wired for monitoring (was: self-sabotaging)

The agentgateway policy would have silently blocked Prometheus scraping `:15020` once observability lands. **Now:** every NetworkPolicy carries the `monitoring` namespace allowance up front.

### D15 — Version/label hygiene

ESS bumped `25.6.1` → `26.6.2` (values revalidated — §2); the LLM model id became a per-cluster `platform-settings` value (the reference runs Vertex AI `google/gemini-2.5-flash` — superseded by D16's multi-provider profiles); the Terraform `bootstrap/` module now exists (it was a phantom path in `mise.toml`).

### D16 — Sovereign model profiles (decided 2026-07-11; implementation: milestone M1)

The reference config hard-wired one hyperscaler model — dissonant with sovereignty-first positioning: the first question every enterprise asks is _"where do our prompts go?"_. **Decision:** the LLM backend is a per-cluster choice through agentgateway (the abstraction already exists — this is configuration surface, not new architecture), with sovereignty-ranked reference profiles: **self-hosted vLLM** (nothing leaves the cluster) > **EU API** (Mistral) > **hyperscalers** (Vertex, Anthropic, OpenAI/Azure — region-pinned where offered). Rules: provider selection lives in `clusters/<env>/platform-settings` + one SOPS secret; token metering and the spend alert must stay provider-agnostic (D7 does not regress); each profile documents its data flow in `docs/models.md`.

**Default decision (2026-07-14):** the production-shaped `local` and `gcp` references select Vertex AI `google/gemini-2.5-flash` through agentgateway's `auth.gcp` boundary. Local k3d projects a cluster-only ADC Secret; Terraform grants the exact GKE agentgateway proxy Workload Identity direct Vertex access without a Google service account or key. Live GKE acceptance remains spend-gated in [#59](https://github.com/fmind-ai/fgentic/issues/59). The credential terminates at agentgateway and never enters an Agent. This is a pragmatic verified-quality and existing-credit choice, not a sovereignty claim: complete model requests and responses leave the cluster for Google, and the selected project, region, contract, retention settings, and billing remain operator controls. The provider-free `demo` overlay stays the out-of-the-box protocol evaluation path, while the self-hosted vLLM profile remains the reference alternative when model traffic must stay inside the cluster.

### D17 — Durable delegation ledger with at-most-once A2A recovery

D3's process-local queue and D4's event marker could still lose work because mautrix acknowledged a transaction before the handler created its job, while the marker suppressed later replay before A2A or Matrix completion. A stable A2A message ID cannot provide distributed exactly-once execution because supported targets do not prove persistent ID deduplication.

**Decision:** atomically store the exact appservice transaction hash and every eligible per-target row before HTTP 200; serialize 32-per-room/256-global non-terminal capacity checks with room rejection first and persist refused targets as content-free terminal evidence; recover accepted work through owner/generation-fenced Postgres leases under database-enforced per-room FIFO; persist a deterministic A2A message ID and attempt boundary before client invocation; treat an outcome-unknown send as terminal `ambiguous` without resend; resume a known task only through `tasks/get`; and project persisted results through deterministic Matrix transaction IDs. Terminal transitions scrub content, ordinary non-content tombstones remain at least 24 hours, and ambiguous/dead evidence remains for operator review. The durable path preserves the stable `queue_full` audit outcome without an unrecorded drop; one ready intake replica remains. See accepted [ADR 0016](adr/0016-durable-delegation-ledger.md) for the exact boundaries, non-durable UX features, and acceptance limits.

### D18 — Permission-aware retrieval binds the projected identity and output audience

An ACL prefilter cannot trust kagent's spoofable `X-User-Id`, and caller-only authorization can still disclose grounded output to other members of a plaintext Matrix room.

**Decision:** keep the knowledge-retrieval service's parameterized database `WHERE` prefilter as the single chunk-row ACL enforcement point. agentgateway ext-auth derives one canonical `X-Fgentic-Identity` projection from either the authenticated Matrix bridge plus the required hot-read room state or the exact validated partner `(issuer, audience, azp)` policy; kagent may propagate only that header to the exact retrieval MCP route. Matrix v1 uses typed full principals with no group mapping; OAuth clients receive only namespaced partner groups. Operator-owned room/client registries project the allowed public or approved-non-public class. Matrix retrieval intersects every current reader's ACL and the bridge rechecks the state digest immediately before content delivery. Every delegation uses a fresh kagent session without caller context, task, or task-reference IDs, while public OAuth retrieval is synchronous and exposes no task read/cancel route. The first implementation relies on exact workload credentials, admission constraints, and enforced NetworkPolicies; it does not claim kagent authenticated the end user or that the plain projection resists a compromised retrieval Agent pod. See accepted [ADR 0017](adr/0017-permission-aware-identity-binding.md) for the protocol contract, room-disclosure boundary, and re-evaluation triggers. Implementation remains tracked by [#333](https://github.com/fmind-ai/fgentic/issues/333); this register entry does not claim the capability is live.

### Workload-identity follow-up

[ADR 0010](adr/0010-defer-spiffe-workload-identity.md) defers SPIRE until both agentgateway's stable Kubernetes API and kagent can consume and authorize an SVID end to end. Installing an issuer while either endpoint still trusts an API key, source IP, or projected header would add an identity control plane without replacing the current boundary.

---
