# Design Decisions D1–D16 (formerly SPEC §4)

> The durable record of _why_ the system looks the way it does. Revisit via a new ADR, never a drive-by PR. Section references `§N` map per the table in [.agents/AGENTS.md](../.agents/AGENTS.md).

Each entry records the problem found in the first draft, the evidence, and what the code/manifests now do. They are kept as the durable record of _why_ the system looks the way it does.

### D1 — Apps depend on infra via Flux Kustomizations (was: broken `dependsOn`)

The draft's bridge **HelmRelease** listed Flux **Kustomizations** in `dependsOn` — but `HelmRelease.spec.dependsOn` only resolves other HelmReleases; the release would wait forever. **Now:** each app is wrapped in its own Flux `Kustomization` (`clusters/base/apps.yaml` → `apps/matrix-a2a-bridge/deploy/`) that `dependsOn` the infra Kustomizations — the DAG stays homogeneous.

### D2 — The reserved ingress IP is pinned by name (was: orphaned)

Terraform reserved a static address the Traefik Service never used; DNS would break on Service recreation. **Now:** the Traefik values carry `networking.gke.io/load-balancer-ip-addresses: fgentic-ingress-ip` (GKE resolves the reserved address by name; harmless on other providers/k3d).

### D3 — Bounded, per-room-ordered dispatch (was: unbounded and unordered)

The appservice API delivers events on a single linearised transaction queue (Synapse #17621 documents the stall mode). mautrix's `EventProcessor` defaults to `AsyncHandlers` (verified in source: one goroutine per event), so a synchronous A2A call in the handler doesn't block the queue — but it means **unbounded concurrent LLM calls with no per-room ordering**. **Now:** `HandleMessage` only classifies and enqueues; a dispatcher drains per-room FIFO queues under a global concurrency cap (`CONCURRENCY`, default 16). The registration sets `rate_limited: false` so ghost replies bypass Synapse's `rc_message` (0.2 msg/s default) — the bridge enforces its own budgets (D7).

### D4 — Postgres-backed bridge state (was: all in-memory)

Pod restarts lost conversation threading, ghost registration state, and the transaction dedup cache — a redelivered transaction **re-invoked agents** (duplicate LLM spend + duplicate replies). **Now:** the `bridge` CNPG database backs the mautrix SQL StateStore and the bridge's own tables (§5); `DATABASE_URL` empty falls back to in-memory for local dev only.

### D5 — Context threads keyed by `(room, agent)` (was: per room)

kagent maps `contextId` 1:1 to a persistent session (verified: `SessionID = task.ContextID`); a room-keyed contextId would collide sessions across two agents in one room and cross-contaminate their memory. **Now:** `bridge_contexts` is keyed `(room_id, ghost)`.

### D6 — Federation-safe target resolution + sender policy (was: localpart spoofing hole)

The draft matched mentions by localpart only — in a federated room, `@agent-k8s:evil.example` would have invoked the _local_ agent; and any room member could invoke any agent. **Now:** a mention resolves to an agent only when its homeserver is the bridge's own server; each agent in `agents.yaml` carries `allowedServers` (extra homeservers, own server always allowed — federated senders **deny-by-default**) and `allowedSenders` (anchored `*`-globs over full user IDs). This is the bridge-level half of the federation trust model (§8), unit-tested.

### D7 — Rate limits as LLM-spend guards (was: none)

Every mention is an LLM invocation; a chatty or malicious room drains the model budget (the closest prior-art project died of exactly this). **Now:** token buckets per `(sender, agent)` (default 6/min, burst 3) and per room (default 30/min, burst 10), rejecting with a polite ghost notice; agentgateway token-unit rate limits on the LLM route are the second layer (§9), and per-consumer quotas arrive with cross-org traffic (§8.3).

### D8 — Loop-safe reply semantics (was: loopable)

A federated agent or third-party bot could `@mention` a local agent whose reply mentions back — unbounded ping-pong. **Now:** ghost replies are **`m.notice`** (the Matrix convention for bot output) and the bridge never treats `m.notice` as a delegating message; the room-level rate bucket (D7) is the backstop.

### D9 — Long tasks via `tasks/get` + Matrix edits (was: hard 60s ceiling)

Real agent work exceeds 60s; kagent deliberately sets no timeout on its agent client and proxies `tasks/get`. **Now:** §6's model — non-terminal `Task` → placeholder reply → poll with backoff up to `TASK_TIMEOUT` (10m) → `m.replace`-edit the placeholder into the final answer. Matrix edits are the deliberate open-standard substitute for streaming; `message/send` stays non-streaming.

### D10 — A2A wire-version pinned to kagent's (was: v2.0.0 vs v2.3.1 drift)

kagent selects the wire format by header, defaulting to legacy 0.3 JSON-RPC, with the default slated to flip around kagent 0.11. **Now:** the bridge pins the SDK version kagent itself runs (v2.3.1); the §12 contract test is the tripwire for the 0.11 flip.

### D11 — kagent treated as unauthenticated (documented, mitigated)

kagent v0.9.11's default `auth.mode: unsecure` derives identity from a spoofable `X-User-Id` header (fallback `admin@kagent.dev`) and its authorizer is a no-op — **NetworkPolicies are the only boundary in front of every agent**. **Now:** a NetworkPolicy on the `kagent` namespace admits only agentgateway, the bridge, kagent's own pods, and monitoring; the bridge stamps `X-User-Id` with the real Matrix sender so kagent sessions/audit attribute to humans; kagent's OIDC (`trusted-proxy` + oauth2-proxy) is tracked for adoption when it stabilizes; any future cross-org exposure goes through agentgateway JWT/mTLS (§8.3), never kagent directly.

### D12 — Data durability (was: zero backups)

A single PVC loss would have destroyed Synapse history, MAS identities, kagent sessions, and bridge state simultaneously. **Now:** CNPG WAL archiving + nightly `ScheduledBackup` to a Terraform-provisioned GCS bucket via a keyless Workload-Identity binding; `instances: 3` documented as the production profile. Still open: the Synapse **media store** decision (S3/GCS media provider vs snapshot-backed PVC) — Phase 1.

### D13 — Supply chain: digest-pinned, signed images (was: mutable `latest`)

**Now:** `cd.yml` builds multi-arch → trivy-scans the pushed digest → cosign-signs (keyless OIDC) → commits the immutable digest into the deploy HelmRelease for Flux to reconcile. CI (`ci.yml`) runs the same `mise` gates as the git hooks plus a clean-tree assertion. Later: Flux `verify.provider: cosign` when the chart moves to OCI.

### D14 — NetworkPolicies pre-wired for monitoring (was: self-sabotaging)

The agentgateway policy would have silently blocked Prometheus scraping `:15020` once observability lands. **Now:** every NetworkPolicy carries the `monitoring` namespace allowance up front.

### D15 — Version/label hygiene

ESS bumped `25.6.1` → `26.6.2` (values revalidated — §2); the LLM model id became a per-cluster `platform-settings` value (the reference runs Vertex AI `google/gemini-2.5-flash` — superseded by D16's multi-provider profiles); the Terraform `bootstrap/` module now exists (it was a phantom path in `mise.toml`).

### D16 — Sovereign model profiles (decided 2026-07-11; implementation: milestone M1)

The reference config hard-wired one hyperscaler model — dissonant with sovereignty-first positioning: the first question every enterprise asks is _"where do our prompts go?"_. **Decision:** the LLM backend is a per-cluster choice through agentgateway (the abstraction already exists — this is configuration surface, not new architecture), with sovereignty-ranked reference profiles: **self-hosted vLLM** (nothing leaves the cluster) > **EU API** (Mistral) > **hyperscalers** (Vertex, Anthropic, OpenAI/Azure — region-pinned where offered). Rules: provider selection lives in `clusters/<env>/platform-settings` + one SOPS secret; token metering and the spend alert must stay provider-agnostic (D7 does not regress); each profile documents its data flow in `docs/models.md`. Vertex remains the verified default until M1 lands.

---
