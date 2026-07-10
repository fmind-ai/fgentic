# SPEC.md — Fgentic Technical Specification

> **What this is.** The binding technical specification for Fgentic: what the platform is, the design decisions and their evidence, the security model, the federation plan, and the roadmap. It began as an adversarial architecture review (2026-07-10) and is now the standing plan; [PLAN.md](PLAN.md) remains the original research record — where they disagree, this SPEC wins.
>
> **Status (2026-07-10).** Design and implementation of the foundation are done: the bridge builds, lints, and passes its unit suite (race-enabled); manifests/Terraform validate; CI/CD exists. **Nothing has been deployed end-to-end yet** — §12 defines the validation ladder, §13 the roadmap.
>
> **Verification note.** Every load-bearing upstream claim was verified against primary sources on **2026-07-10** (kagent and agentgateway cloned and inspected at HEAD; ESS 26.6.2 values schema pulled and diffed; licenses read from LICENSE files; foundation policies read from governance repos; homeserver candidates researched across releases, issue trackers, and funding sources).

---

## 1. Vision & Goals

1. **What Fgentic is.** An open-source, self-hostable, **federated** agent-collaboration platform: humans and AI agents co-inhabit Matrix rooms; `@mention` delegates a task over A2A to an agent hosted on kagent, governed by agentgateway; and — the differentiating goal — **organizations federate**, so agents and humans from different companies collaborate in shared rooms across org boundaries without any proprietary SaaS in the loop.
1. **Why it matters.** The closed platforms (Microsoft Entra Agent ID + Teams, Google's agent ecosystem, Slack AI) are tenant-anchored and vendor-owned. No open, production-ready alternative for _cross-organization_ agent collaboration exists in mid-2026 (verified: AGNTCY, NANDA, ANP are all pre-production; the A2A registry is a roadmap item). Matrix federation is the only battle-tested cross-org messaging federation in production (Germany's TI-Messenger: 150k+ healthcare orgs; NATO NI2CE; Sweden's public sector; France's Tchap). Fgentic composes that proven fabric with the emerging agent standards (A2A, MCP) — a genuinely unoccupied niche: **no other Matrix↔A2A appservice bridge exists** (closest prior art: MindRoom, Beeper ai-bridge, baibot — all direct-LLM bots, none A2A).
1. **Adoption target.** Long-term donation to the **Agentic AI Foundation (AAIF)**. Realism check (verified against [AAIF's lifecycle policy](https://github.com/aaif/project-proposals)): AAIF has no low-bar sandbox — its entry (Growth) stage already requires wide production adoption, a TC sponsor, and maintainer diversity. The achievable path is: build adoption → **CNCF Sandbox** (kagent's path; low bar: Apache-2.0, DCO, trademark transfer) → AAIF when adoption warrants it. Both paths effectively require **Apache-2.0** (§10) and vendor-neutral open governance (DCO, public tracker, GOVERNANCE.md when contributors arrive).
1. **Non-goals.** Not an agent framework (kagent owns that), not a gateway (agentgateway owns that), not a Matrix distribution (ESS/others own that). Fgentic is the _composition_: the bridge, the manifests, the federation profile, the runbooks, and the reference deployment (`fgentic.fmind.ai`).

### 1.1 Naming (decided 2026-07-10)

The product name is **Fgentic** ("federated + agentic" — the name literally encodes the value proposition); the repo/artifact slug is `fgentic`. The reference deployment lives at **`fgentic.fmind.ai`** (the Matrix server_name; hosts `chat.`/`matrix.`/`auth.fgentic.fmind.ai`) in the GCP project **`fgentic-ai`**. A vendor-neutral name is also a foundation-donation prerequisite (trademark transfers to LF).

---

## 2. Architecture: current, verified state

The layering in [PLAN.md §3](PLAN.md) stands. This matrix is the source of truth where PLAN.md's 2026-07-08 snapshot drifted:

| Layer         | Component                       | Verified state (2026-07-10)                                                                                                                                                                                             | Load-bearing facts                                                                                                                                                                                                                                                                                            |
| ------------- | ------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Fabric        | Matrix spec                     | **v1.19**; room version 12 default since v1.16                                                                                                                                                                          | Room v12 is mandatory for federation (Project Hydra state-reset fix, CVE-2025-49090 class). MSC4186 (sliding sync) is de-facto standard for Element X, accepted by the SCT July 2026.                                                                                                                         |
| Homeserver    | Synapse + MAS via ESS Community | chart **26.6.2** (pin was 13 months stale; values revalidated against the 26.x schema — external-Postgres `password:{secret,secretKey}`, appservice `{secret,secretKey}` entries, well-known additions as JSON strings) | The homeserver decision + alternatives + switch triggers: §10.3. The bridge's API surface (registration, `as_token`, `user_id` masquerading) is stable-spec and MAS-proof (MSC3861 does not touch appservice tokens) — the platform is homeserver-portable by construction.                                   |
| Delegation    | A2A protocol                    | **spec v1.0.0** (April 2026): mTLS, OAuth2/OIDC security schemes, **Signed AgentCards** (JWS)                                                                                                                           | v1.0's signed cards + mTLS are load-bearing for the federation design (§8). A2A is a standalone Linux Foundation project (not AAIF).                                                                                                                                                                          |
| A2A SDK       | `a2a-go/v2`                     | Bridge pins **v2.3.1** — the same version kagent runs                                                                                                                                                                   | kagent negotiates wire version via header, defaulting to legacy 0.3 JSON-RPC when absent; that default is slated to change around kagent 0.11 (`TODO(0.11.0)` in kagent's `a2a_version.go`) — pinned by the contract test planned in §12.                                                                     |
| Agents        | kagent                          | v0.9.11 (CNCF Sandbox; incubation review filed 2026-03)                                                                                                                                                                 | **Requires PostgreSQL** (bundled chart DB is dev-only) — provisioned as the `kagent` DB in CNPG. A2A `contextId` maps **1:1 to a persistent kagent session** (`SessionID = task.ContextID`), which validates the bridge's threading design. Its A2A endpoint is **unauthenticated by default** (§7).          |
| Data plane    | agentgateway                    | v1.3.1 (Apache-2.0, **hosted by AAIF since June 2026**)                                                                                                                                                                 | On the A2A hop it proxies + rewrites AgentCards + logs `a2a.method` only — no A2A response inspection, no per-agent RBAC (per-path CEL authorization can approximate it). Watch: GHSA-jwm2-83f3-52xc (cross-namespace backend without ReferenceGrant) published 2026-06-29, _after_ v1.3.1 — bump when fixed. |
| Bridge        | matrix-a2a-bridge (this repo)   | mautrix/go v0.28.1 (**MPL-2.0**, not AGPL) + a2a-go v2.3.1; unit-tested, race-clean                                                                                                                                     | Design decisions D1–D15 (§4) are implemented; behavior specified in §5–§6.                                                                                                                                                                                                                                    |
| State         | CloudNativePG                   | operator chart 0.29.0, PG 17; WAL archiving + nightly base backups to GCS via Workload Identity                                                                                                                         | `instances: 1` for the reference; 3 for production. Database-per-service: `synapse` (C collation), `mas`, `bridge`, `kagent`.                                                                                                                                                                                 |
| Delivery      | Flux v2 + GitHub Actions        | Apps wrapped in Flux Kustomizations; CI (`ci.yml`) runs the mise gates; CD (`cd.yml`) builds → scans → signs → pins digests                                                                                             | `HelmRelease.dependsOn` only resolves HelmReleases — apps depend on infra via their wrapping Kustomization (D1). Images deploy by immutable digest, never `latest`.                                                                                                                                           |
| Observability | (not yet built)                 | —                                                                                                                                                                                                                       | Specified in §9; the NetworkPolicies already carry the `monitoring` allowances so nothing rots.                                                                                                                                                                                                               |

---

## 3. Core Data Flow

The `@mention → A2A → reply` flow of PLAN.md §3.1 stands, amended by the async delegation model (§6): the bridge ACKs the Matrix transaction immediately, dispatches the A2A call on a bounded per-room-ordered worker pool, and posts/edits the ghost reply when the task completes — the appservice transaction queue is never blocked and LLM concurrency is always bounded.

---

## 4. Design Decisions from the Adversarial Review (all applied 2026-07-10)

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

ESS bumped `25.6.1` → `26.6.2` (values revalidated — §2); model id updated to `claude-sonnet-5`; the Terraform `bootstrap/` module now exists (it was a phantom path in `mise.toml`).

---

## 5. Bridge State Management (as implemented)

1. **Storage:** the `bridge` CNPG database; one shared `dbutil` pool (pgx driver) backs both the mautrix SQL StateStore and the bridge's own tables. Schema creation is idempotent DDL at startup — two tables don't justify a migration framework.
1. **Tables:** `mx_*` (mautrix StateStore, schema owned/upgraded by mautrix); `bridge_contexts(room_id, ghost, context_id, updated_at, PK(room_id, ghost))`; `bridge_processed_events(event_id PK, processed_at)` pruned opportunistically past 24h.
1. **Semantics:** context rows are best-effort (loss degrades to a fresh conversation, never an error). Event dedup is insert-if-absent **before** dispatch, so Synapse's at-least-once transaction delivery collapses to effectively-once agent invocation; on a store error the bridge proceeds (a rare duplicate beats a dropped delegation).
1. **HA note:** with Postgres-backed dedup and per-room ordering keyed in one process, `replicas: 1` is the design point (the appservice protocol is single-consumer); resilience is homeserver-side transaction retry + fast pod rescheduling, not replicas.

## 6. Async Delegation (as implemented)

1. `HandleMessage`: drop own bot/ghost senders and anything but `m.text` (D8) → resolve targets under D6's rules → dedup by event ID (D4) → enqueue `(room, ghost, prompt)` per target; return immediately.
1. Worker (per-room FIFO, global cap): ensure ghost registered/joined → rate check (D7, polite `m.notice` on rejection) → **typing indicator** while the agent works (cleared on exit) → `message/send` with the `(room, ghost)` contextId, the Matrix sender in `X-User-Id`, and a `REQUEST_TIMEOUT` (60s) transport deadline.
1. Terminal `Message`/`Task`: post the extracted text (artifacts → status message → last agent turn) as the ghost — an **`m.notice` reply** to the original event.
1. Non-terminal `Task`: post a "⏳ working on it…" placeholder → poll `tasks/get` (2s → 15s exponential backoff, 3-error budget, overall `TASK_TIMEOUT` 10m) → **edit** (`m.replace`) the placeholder into the final answer.
1. Failures post a short, generic ghost message (internal endpoints/errors never leak into rooms) and log the full error; empty replies get an explicit "(the agent returned no content)".

## 7. Security Boundaries & Threat Model

Trust zones, outermost first:

| Zone                                     | Trust level                   | Controls                                                                                                                                                                                                                                                                                         |
| ---------------------------------------- | ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Internet → Gateway (Element/Synapse/MAS) | Untrusted                     | Gateway API + TLS; MAS OIDC; Synapse rate limits; federation **disabled** until Phase 6                                                                                                                                                                                                          |
| Federated homeservers (Phase 6)          | Semi-trusted (contract-bound) | §8 controls: allowlist federation, Server ACLs, room v12, sender policies in the bridge (D6)                                                                                                                                                                                                     |
| Room content → agents                    | **Untrusted input**           | Prompt injection is the #1 unsolved threat (OWASP LLM01; "Prompt Infection" arXiv:2410.07283; EchoLeak CVE-2025-32711 as precedent). Mitigations: sender allowlists (D6), least-privilege agent tools, human-in-the-loop for consequential actions, agent output treated as data by other agents |
| Bridge → kagent A2A                      | Cluster-internal              | NetworkPolicies on **both** agentgateway and kagent namespaces (D11); kagent endpoint is otherwise unauthenticated                                                                                                                                                                               |
| Agents → LLM                             | Governed egress               | agentgateway is the single credential holder; prompt guards + token rate limits (§9); pods have no model keys                                                                                                                                                                                    |
| Secrets                                  | —                             | SOPS+age only; per-service DB roles; registration tokens never in git plaintext                                                                                                                                                                                                                  |

Standing rules: agent rooms unencrypted **within one org** stays acceptable (ADR 0008) but its rationale **does not survive federation** — revisit is mandatory in Phase 6 (§8.4). All images distroless/non-root; PSS `restricted` on app namespaces.

## 8. Federation Spec (Phase 6 — the flagship differentiator)

Design position: **Matrix federation for the collaboration plane (humans + agents in shared rooms), A2A for the delegation plane (direct org-to-org machine calls)** — they compose rather than compete, and Fgentic ships both:

### 8.1 What Matrix federation gives — stated honestly

1. Identity is **org-level, not agent-level**: events are signed by the homeserver; org B can forge any `@user:org-b.com`, including its own agents. The honest claim is "cryptographically attributable to the partner organization". Per-agent identity is layered on with **A2A v1.0 Signed AgentCards** on the delegation plane.
1. Room state and history replicate **fully and irrevocably** to every participating homeserver; redaction cross-server is best-effort ("gentlemen's agreement" — Matrix Foundation's own words). Data residency is a **contractual** control (DPA + mutual retention policy), not a technical one. The spec says so out loud; enterprises respect honesty about this more than silence.

### 8.2 Required hardening (all git-declared)

1. **Room version 12 minimum** for any federated room (Hydra fix for malicious state resets — CVE-2025-49090 class).
1. **Closed federation**: Synapse `federation_domain_whitelist` (mutual, includes own domain + a reachable key notary), federation listener firewalled to partner IPs where feasible.
1. **Server ACLs** (`m.room.server_acl`) allowlisting partner servers per room; `m.federate: false` on rooms that must never federate (it is immutable at creation — set it deliberately, always).
1. **Synapse module callbacks** (`federated_user_may_invite`, `should_drop_federated_event`) as the programmatic policy border — this is Fgentic's open equivalent of the "Secure Border Gateway" that TI-Messenger mandates between parties (and which Element gates behind ESS Pro; ours is policy-as-code instead of a paid appliance).
1. **Bridge sender policy** (D6): per-agent `allowedServers`/`allowedSenders`; federated senders deny-by-default — already implemented and tested, ahead of Phase 6.

### 8.3 Cross-org delegation plane

When org B's agents should be _invoked_ (not just conversed with): expose selected A2A endpoints through agentgateway on a dedicated Gateway listener with **JWT (OIDC federation between orgs) or mTLS** per A2A v1.0 security schemes, Signed AgentCard published at the well-known path, per-consumer token quotas. The bridge's agents map gains a `url:` variant (today it can only address kagent `namespace/name`) so remote signed agents become mentionable ghosts too.

### 8.4 E2EE revisit (supersedes ADR 0008's scope)

For federated rooms, "unencrypted" means the partner's server operators read everything, forever. Options, in order of preference: (a) keep federated agent rooms plaintext but **scoped** — dedicated per-project rooms, no sensitive-room federation, contractual controls (ship this first; it is what TI-Messenger-style deployments do in practice); (b) adopt appservice E2EE (mautrix crypto) later if demanded — acknowledged as officially "not recommended" and config-heavy. Document (a) as ADR 0008-bis when Phase 6 starts.

## 9. Observability Spec (next up — `infra/observability/`, not yet built)

1. **Metrics:** kube-prometheus-stack (Prometheus + Alertmanager + Grafana). agentgateway ships ServiceMonitors + a Grafana dashboard (control plane and per-proxy GenAI metrics: `gen_ai_client_token_usage`, `gen_ai_client_cost`, TTFT/TPOT); kagent exposes authenticated controller `/metrics`; CNPG enables `monitoring.enablePodMonitor` (stub already commented in `cluster.yaml`, gated on the Prometheus Operator CRDs). License note: Grafana and Loki are AGPL-3.0 (fine to run unmodified, documented as swappable — VictoriaMetrics/VictoriaLogs are the Apache-2.0 alternates for AGPL-banning shops; Prometheus/OTel/Jaeger are Apache-2.0).
1. **Traces:** OTel Collector; enable kagent `otel.tracing` (OTLP) and agentgateway OTLP export; **instrument the bridge** with OTel (span per delegation: Matrix event → A2A send → poll → reply post) propagating W3C `traceparent` so one trace covers mention → gateway → agent → LLM. Backend: Jaeger (Apache-2.0) over Tempo (AGPL) by default.
1. **Bridge metrics:** Prometheus `/metrics` on a side port — delegations total/by agent, A2A latency histogram, queue depth, dedup hits, rate-limit rejections. Also structured audit logs (who invoked what, from which room/server).
1. **Agent evaluation:** MLflow (Apache-2.0, LF) as the optional eval/experiment store, fed by OTel GenAI traces; deployed off by default (own namespace + DB) — it is analysis tooling, not a runtime dependency.
1. **LLM cost:** agentgateway cost catalogs + `gen_ai_client_cost` are the budget dashboard — wire an Alertmanager rule on spend rate (the failure mode that killed the closest prior-art project).
1. NetworkPolicies already admit the `monitoring` namespace everywhere (D14).

## 10. Licensing & Foundation Strategy

### 10.1 Project license — **Apache-2.0** (decided and applied 2026-07-10)

The repository is licensed **Apache-2.0** (was MIT in the first draft). Justification (all verified):

1. **Foundation fit.** AAIF requires an "OSI-approved permissive license" (Apache-2.0 in practice — all four hosted projects use it); CNCF's charter default is Apache-2.0. Either donation path is friction-free under Apache-2.0, needs an exception under MIT/MPL.
1. **Patent grant.** Apache-2.0 §3 grants and defensively terminates patent rights — the property enterprise counsel checks first on an _agent-infrastructure_ platform; MIT has none.
1. **Stack coherence.** A2A, MCP, kagent, agentgateway, CloudNativePG, Flux, cert-manager are all Apache-2.0.
1. **Cost.** Trivial now (sole author); painful after external contributions. Done before the first outside PR. **DCO** (not CLA) — matches kagent/agentgateway and avoids the asymmetric-rights problem Element's CLA exemplifies.
1. **Obligation:** the bridge binary embeds `maunium.net/go/mautrix` (**MPL-2.0** — verified, not AGPL). The `NOTICE` files (repo root + app, shipped in the image) state that and link the source. That is MPL's entire ask for unmodified use.

### 10.2 The honest AGPL/open-core map (the "no strings attached" audit)

| Component                          | License                                         | Verdict for Fgentic                                                                                                                                                                                                                                                                                                                                                                                                    |
| ---------------------------------- | ----------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Synapse, MAS, Element Web/X        | AGPL-3.0 OR Element-Commercial (CLA-encumbered) | Legally clean to self-host **unmodified** (AGPL §13 triggers on modification only; deploying via manifests conveys nothing). But many enterprises blanket-ban AGPL (Google policy, CNCF allowlist) — **documented as swappable** (§10.3).                                                                                                                                                                              |
| **ESS Community (ess-helm)**       | AGPL-3.0 chart, **open-core funnel**            | README says "non-commercial… up to 100 users… not for production in commercial environments" — that is _positioning, not license terms_ (AGPL can't restrict commercial use), but HA/autoscaling, Secure Border Gateway, audit bots, LDAP, Synapse Pro are genuinely Pro-gated. Stated openly in the README rather than papered over; §8.2's policy-as-code border is the open answer to the Pro-gated border gateway. |
| mautrix/go (library)               | **MPL-2.0**                                     | The load-bearing good news: the bridge stays permissive. NOTICE shipped (§10.1).                                                                                                                                                                                                                                                                                                                                       |
| mautrix bridges (Slack/Telegram/…) | AGPL-3.0                                        | Deployment-only artifacts (phase 4), never linked — fine, same "unmodified" logic.                                                                                                                                                                                                                                                                                                                                     |
| Grafana, Loki, Tempo               | AGPL-3.0 (Grafana Labs CLA)                     | Default observability stack, with documented Apache-2.0 alternates (VictoriaMetrics/VictoriaLogs, Jaeger).                                                                                                                                                                                                                                                                                                             |
| Everything else in the stack       | Apache-2.0 / MIT                                | No caveats relevant to Fgentic.                                                                                                                                                                                                                                                                                                                                                                                        |

### 10.3 Homeserver strategy (deep dive concluded 2026-07-10 — **keep ESS, don't switch**)

Two supported profiles behind the same Helm layer; a full candidate comparison (ESS-less Synapse, Tuwunel, continuwuity, Conduit, Palpo) was researched against primary sources on 2026-07-10:

1. **Reference profile (default): ESS Community, pinned 26.6.2.** Nothing in mid-2026 beats Synapse+MAS+Element on the platform's top requirements — appservice API maturity (the bridge's lifeline), MAS/MSC3861 + Element X, federation policy hooks (spam-checker module callbacks, the basis of §8.2's border-policy design), and PostgreSQL (the only candidate that fits the shared CNPG cluster at all). Dropping the ess-helm chart for community charts (ananace + a separate MAS chart) yields **zero licensing gain** — the AGPL + open-core funnel lives in Synapse/MAS/Element themselves, not the chart — at real maintenance cost. The stale pin was the actual problem, and it is fixed.
1. **Pure-permissive profile: Tuwunel first, continuwuity as the community alternate** (both Apache-2.0, both active). Tuwunel: monthly releases, built-in MSC3861 OIDC + explicit MAS integration (v1.8.0), battle-tested MSC4186 (Element X), the only nation-scale non-Synapse production deployment (Swiss government sponsorship) — but bus factor ≈ 1 (one maintainer ≈ 85% of commits), four appservice regressions shipped in 2026 alone, admin-room appservice registration (no Secret-mounted file), **RocksDB only** (no Postgres — this profile deviates from ADR 0007 with a PVC + RocksDB backup engine), and no maintained Helm chart. continuwuity: healthier contributor spread (~15 humans) and richer federation allowlist knobs, but deliberately no MAS path, OIDC still alpha, zero institutional funding. The two forks' databases are mutually incompatible — the profile choice is a one-way door once data exists. Watch-list: **Palpo** (Rust + PostgreSQL, Apache-2.0) is the only future candidate that would satisfy CNPG, still pre-production.
1. **Switch triggers (do not switch preemptively):** (a) Element moves a Community capability the platform uses (appservice loading, MAS wiring, well-known, non-worker Synapse) behind Pro or discontinues ESS Community; (b) a deployment hard-bans AGPL → activate the pure-permissive profile _for that deployment_; (c) Tuwunel/continuwuity ship Postgres + a maintained chart + 6 regression-free months on the appservice API → re-evaluate the default. Migration cost if triggered: the bridge survives nearly intact (spec-only API; only the registration delivery mechanism changes); MAS and the `synapse`/`mas` CNPG databases are orphaned; accounts/history are recreated, not migrated (no tooling exists).

Rules regardless of profile: never mirror AGPL images into project registries (reference upstream only — redistribution is what triggers source-offer duties), never fork AGPL components in-repo.

## 11. Terraform / GCP Reference (applied 2026-07-10)

`infra/terraform/` now implements: remote state (versioned GCS bucket via the one-time `bootstrap/` module + `backend "gcs"`), **private nodes + Cloud NAT** (nodes have no public IPs; the control-plane endpoint stays public but CIDR-allowlisted, never `0.0.0.0/0`), the named static ingress address (pinned by Traefik — D2), the CNPG **backups bucket + keyless Workload-Identity GSA**, a `regional` toggle (zonal for the disposable reference, regional for production SLA), optional Cloud DNS A records (`manage_dns` + `dns_zone_name`), a weekly maintenance window, `pd-balanced` node disks, explicit legacy-metadata-endpoint disable, Workload Identity, Dataplane V2 (NetworkPolicy enforcement), and shielded nodes with a least-privilege node SA.

Deliberately **not** defaulted on (the "regulated-industry" tier, documented only): CMEK/etcd encryption, Binary Authorization, Backup for GKE. Worth adding when a Vertex/Gemini backend is used: bind agentgateway's KSA via Workload Identity instead of an API-key Secret — the chokepoint becomes credential-_less_.

Bootstrap order: apply `bootstrap/` first (local state, creates `fgentic-ai-tfstate`), then `terraform init -migrate-state` in the main module. Bucket names are global — `fgentic-ai-tfstate` / `fgentic-ai-pg-backups` each live in one place if they must change (bootstrap variable; `pg_backups_bucket_name` + `infra/postgres/cluster.yaml`).

## 12. Testing & Validation Ladder

1. **Unit — done:** 28 tests, race-enabled, covering target resolution (federation spoofing, sender policy, dedup), dispatch ordering/concurrency/shutdown, rate-limit config, state semantics, and the `Task|Message` result mapping incl. non-terminal tasks. `mise run check` (lint, vuln, format, secrets, manifests via kubeconform + helm template, terraform validate, actionlint, trivy config) is clean.
1. **Contract — next:** an in-process A2A server fixture using `a2asrv` (the SDK's server half) exercising `Message` vs terminal-`Task` vs working-`Task` results, contextId echo, and kagent's wire-version header behavior — the permanent tripwire for D10.
1. **Integration (local k3d):** Skaffold profile standing up Synapse (lightweight config or ESS), registering the appservice, running the bridge with Postgres, and driving a scripted `@mention → reply` via the client API. Runs in CI on a `kind` runner.
1. **E2E demo script (Phase 5):** the runbook-driven "enterprise showcase" path: bootstrap → login → room → mention → reply → one Grafana trace covering the full hop. The demo _is_ the acceptance test.
1. **Load sanity:** N concurrent mentions across M rooms proving D3's queue design (no txn timeouts, bounded memory, per-room order held).

## 13. Roadmap

1. **Phase 0 — Foundations. DONE** (2026-07-10): repo scaffold, Apache-2.0 + NOTICE, all review fixes D1–D15 (including the bridge work originally scheduled for Phase 3), Terraform hardening (§11), CI/CD (D13), Fgentic naming.
1. **Phase 1 — Matrix layer, first deployment:** apply `bootstrap/` + Terraform; create the SOPS secrets from the `*.example` templates; DNS + TLS for `fgentic.fmind.ai` (+`chat.`/`matrix.`/`auth.`); reconcile ESS 26.6.2 against shared CNPG; decide Synapse media storage (D12 residue); verify Element Web login and a plain room.
1. **Phase 2 — Agent layer + observability:** agentgateway (LLM chokepoint + A2A route) and kagent with the sample agents; **build §9** (`infra/observability/`) now so every later phase is measurable; verify `a2a send` from the shell through the gateway.
1. **Phase 3 — Bridge live:** deploy the bridge (registration Secret shared with ESS, `bridge` DB); contract + integration tests (§12); verify `@mention → A2A → reply`, threading, long-task edits, rate limits end-to-end.
1. **Phase 4 — Interop bridges (on demand):** off-the-shelf mautrix bridges (own appservice + own DB each), clean-ToS networks first; Element X via MAS.
1. **Phase 5 — Hardening + demo:** load sanity, dashboards, runbooks, the recorded end-to-end demo — the enterprise-showcase narrative.
1. **Phase 6 — Federation (the thesis):** §8 in full — partner-org profile (room v12, closed federation, Server ACLs, module-callback border), ADR 0008-bis, cross-org A2A via agentgateway with Signed AgentCards, and a two-org reference demo. Everything before this phase must not paint it into a corner (D6 is why that rule exists).
