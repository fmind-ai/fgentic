# PLAN.md — Fgentic: An Open-Standard AI Agent Collaboration Platform

> **What this is.** A reference, open-source platform that shows enterprises how to build an **interoperable AI-agent platform on open standards** — where humans and AI agents share the same chat rooms, address each other by `@mention`, and delegate tasks to one another over a mature, vendor-neutral protocol stack. It runs entirely on Kubernetes and is composed only of open standards and open-source components: **Matrix** (collaboration fabric + UI), **A2A** (agent-to-agent delegation), **kagent** (agents on Kubernetes), **agentgateway** (the AI-native data plane / governance chokepoint), and a small **Go bridge** that stitches Matrix to A2A.
>
> Unlike its sibling `dev.fmind` (a cost-capped free-tier mini-cloud), **Fgentic deliberately does not optimise for billing or a single tiny node** — it optimises for demonstrating the _complete, enterprise-credible_ pattern. It still runs on standard Kubernetes and stays as provider-independent as possible.
>
> This document is the durable record of the research, the decisions, and the build plan. Every load-bearing claim was verified against primary sources (upstream repos at HEAD and specs) on **2026-07-08**; versions and sources are cited inline so the plan can be re-verified on upgrade.

---

## 1. The Idea (and why it is worth building)

Agent frameworks (kagent) and AI data planes (agentgateway) are excellent at _creating and governing_ AI assets — LLMs, MCP tools, A2A agents. What they lack is a **good collaboration UI and a human↔agent interaction surface**. The proposal:

1. **Humans and agents co-inhabit chat rooms.** Every agent is a first-class room member with its own identity (`@agent-k8s:fgentic.fmind.ai`).
1. **`@mention` is the delegation primitive.** A human (or another agent) addresses an agent with `@agent-name`; that is the universal "please take this" gesture, already understood by every chat user on earth.
1. **The delegation travels over A2A.** When a message targets an agent, a bridge relays it to that agent's **A2A endpoint** (`message/send`, non-streaming — fire-and-forget request/response) and posts the reply back into the room. No custom streaming, no bespoke API.
1. **Interoperability comes for free, later.** Because Matrix is a _bridged_ protocol, the same rooms can later connect to WhatsApp, Slack, Signal, Telegram, etc. via mature Matrix bridges — turning "chat with my agents" into "chat with my agents _and_ anyone on any network."

**Why this is a genuinely good idea for an enterprise showcase:** it is built entirely on **open, mature, governed standards** — Matrix (an [IETF-track](https://matrix.org), foundation-governed protocol used by NATO, the French/German governments, and Sweden's eSam), A2A (a **Linux Foundation** protocol), MCP (Linux Foundation / Agentic AI Foundation), and Kubernetes. There is **no proprietary SaaS in the loop**, no vendor lock-in, and every layer is swappable. That is exactly the value proposition an enterprise building an agent platform wants to see de-risked.

**The honest counter-point (recorded so nobody has to rediscover it):** for a _single solo developer_ who only wants "let me chat with my agents," standing up a full homeserver is over-engineering — kagent already ships Slack/Discord/Telegram→A2A bridge examples that deliver that UX with zero homeserver. Matrix earns its keep precisely when **(a) multiple humans**, **(b) self-hosted/sovereign** operation, and **(c) multi-network interoperability** are real requirements. This project is the reference build for **exactly that** case — it is the "yes, build Matrix" answer made concrete. See [ADR 0001](docs/adr/0001-open-standard-agent-platform.md).

---

## 2. Feasibility — verified, not assumed

Every hop of the concept is **real, currently-shipping code** (verified against upstream HEAD on 2026-07-08):

| Layer                | Component                                        | Version (2026-07-08)                                             | Verified capability                                                                                                                                                               |
| -------------------- | ------------------------------------------------ | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Collaboration fabric | **Synapse** (via Element Server Suite Community) | ESS `matrix-stack` chart                                         | Reference homeserver; full Application Service API; native Simplified Sliding Sync (MSC4186) for Element X.                                                                       |
| Modern auth          | **Matrix Authentication Service (MAS)**          | ships in ESS                                                     | OIDC/OAuth2 (MSC3861) — required by Element X, enables SSO/enterprise IdP.                                                                                                        |
| Clients              | **Element Web** + **Element X** (iOS/Android)    | latest                                                           | Rooms, `@mentions` (m.mentions / MSC3952), formatted replies. Element Web needs neither MAS nor sliding sync; Element X needs both — ESS provides both.                           |
| Bridge (the glue)    | **mautrix/go** appservice framework              | `v0.28.1`                                                        | Namespace registration for `@agent-.*`, multi-ghost puppeting via `as.Intent()`, typed `m.mentions`. Powers Beeper + every mautrix bridge.                                        |
| Delegation protocol  | **A2A** + `a2a-go` v2                            | spec v0.3.0 (proto "1.0"); SDK `github.com/a2aproject/a2a-go/v2` | Non-streaming `message/send` returns a synchronous `Task \| Message`. AgentCard discovery at `/.well-known/agent-card.json`.                                                      |
| Agents               | **kagent**                                       | `v0.9.x` (chart `0.9.11`)                                        | Each declarative Agent with `a2aConfig` is an A2A server at `http://kagent-controller.kagent:8083/api/a2a/<ns>/<name>` with an AgentCard. Non-streaming `message/send` supported. |
| AI data plane        | **agentgateway**                                 | `v1.3.x` (v1.0.0 shipped 2026-03)                                | First-class A2A routing (`AgentgatewayBackend{a2a:{host,port}}` + HTTPRoute); single model-credential chokepoint for LLM egress.                                                  |
| Interop (later)      | **mautrix bridges**                              | `v26.04` on mautrix-go                                           | Slack/Telegram/Signal/WhatsApp/… as separate appservices; Beeper-grade, all Go.                                                                                                   |

**The one novel piece is the bridge**, and it is small: mautrix/go gives the entire Matrix side (namespace registration, event transport, ghost puppeting, typed mentions) and `a2a-go` gives the entire A2A side (card discovery, typed `SendMessage`). The code we own is: a registration file, an `@agent → (namespace,name)` map, a `SendMessageResult → string` extractor, and the reply wiring. See [`apps/matrix-a2a-bridge/`](apps/matrix-a2a-bridge/).

---

## 3. Target Architecture

```text
                                   ┌─────────────────────────────────────────────┐
   Humans (Element Web / X) ──────▶│  Gateway API (Traefik) + cert-manager TLS    │
   @agent-k8s "why is pod X down?" │  chat. / matrix. / auth. .fgentic.fmind.ai       │
                                   └───────────────┬─────────────────────────────┘
                                                   │ (Matrix Client-Server API, HTTPS)
                                   ┌───────────────▼─────────────────────────────┐
                                   │  Matrix layer  (ns: matrix)                  │
                                   │  Element Web · Synapse (homeserver) · MAS    │
                                   │  well-known delegation for fgentic.fmind.ai      │
                                   └───────┬───────────────────────▲──────────────┘
             AS transaction push (PUT      │                       │ reply posted as ghost
             /_matrix/app/v1/transactions) │                       │ (@agent-k8s, m.relates_to)
                                   ┌────────▼───────────────────────┴──────────────┐
                                   │  matrix-a2a-bridge  (ns: bridge)  [Go]         │
                                   │  appservice: @agent-.* ghosts + @a2a-bridge    │
                                   │  detect @mention → map → A2A message/send      │
                                   │  ClusterIP + NetworkPolicy (not internet-exposed) │
                                   └────────┬───────────────────────────────────────┘
                                            │ A2A JSON-RPC message/send (non-streaming)
                                   ┌────────▼───────────────────────────────────────┐
                                   │  agentgateway  (ns: agentgateway-system)        │
                                   │  A2A route → kagent; LLM/MCP egress chokepoint  │
                                   │  (single model credential lives here)           │
                                   └────────┬───────────────────────────▲────────────┘
                        A2A → /api/a2a/<ns>/<name>                       │ agent's LLM egress
                                   ┌────────▼───────────────────────────┴────────────┐
                                   │  kagent  (ns: kagent)                            │
                                   │  Agent CRDs (a2aConfig) served as A2A on :8083   │
                                   │  k8s-agent, helm-agent, … each an AgentCard      │
                                   └──────────────────────────────────────────────────┘

   Shared state:  CloudNativePG (ns: postgres) — databases: synapse, mas, bridge
   Delivery:      Flux v2 pull-based GitOps  ·  Local dev: k3d + Skaffold
   Later interop: mautrix-slack / -telegram / … as additional appservices (ns: bridges)
```

### 3.1 The `@mention → A2A → reply` data flow (step by step)

1. A human types in an Element room: `@agent-k8s why is pod X crashing?`. Element populates the typed `m.mentions.user_ids` field (MSC3952) with `@agent-k8s:fgentic.fmind.ai`.
1. Synapse receives the `m.room.message` event and, because `@agent-.*` is in the bridge appservice's **exclusive** user-ID namespace, pushes it via `PUT /_matrix/app/v1/transactions/{txnID}` to the Go bridge (ClusterIP, behind a NetworkPolicy).
1. The bridge's mautrix `EventProcessor` dispatches `event.EventMessage`; the handler reads `evt.Content.AsMessage().Mentions.UserIDs`, matches the `@agent-.*` regex (with a plaintext-body fallback for clients that omit `m.mentions`).
1. The bridge maps `@agent-k8s → (namespace=kagent, name=k8s-agent)` via an allowlist config, optionally validating the target by fetching/caching its AgentCard from `…/.well-known/agent-card.json`. Unknown targets are rejected fast.
1. The bridge builds `a2a.NewMessage(user, NewTextPart(body))` with a per-room `contextId` (stored `roomID → contextId` for multi-turn threading), sets a 60s deadline, and calls `a2aclient.SendMessage` — routed to the **agentgateway** ClusterIP, path `/api/a2a/kagent/k8s-agent` (or direct to `kagent-controller:8083`, configurable).
1. agentgateway logs the JSON-RPC method (telemetry) and forwards to kagent's A2A passthrough handler. kagent runs the agent; **the agent's own LLM calls egress back out through agentgateway**, where the single model credential lives.
1. kagent returns a `SendMessageResult` synchronously (a completed `*Task` or a bare `*Message`). The bridge type-switches the sum type and extracts the text into a plain string.
1. The bridge calls `as.Intent(@agent-k8s:fgentic.fmind.ai)` → `EnsureRegistered(ctx)` → `EnsureJoined(ctx, roomID)` → send a message with an `m.relates_to` reply pointing at the original event. The reply posts into the room **as the ghost user** and the human reads it in Element. (For slow agents, an optional "working…" notice is posted first.)

---

## 4. Component Decisions (with rationale and rejected alternatives)

### 4.1 Homeserver + Auth + Client → **Synapse + MAS + Element, via Element Server Suite (ESS) Community**

- **Decision.** Deploy the Matrix layer with **[ESS Community](https://github.com/element-hq/ess-helm)** (`element-hq/ess-helm` `matrix-stack`), which bundles **Synapse** (reference homeserver, best-in-class Application Service + bridge support), **MAS** (Matrix Authentication Service — OIDC/OAuth2, MSC3861), **Element Web**, and **well-known delegation** in a single, Element-maintained Helm chart. See [ADR 0003](docs/adr/0003-synapse-mas-element-ess.md).
- **Why (enterprise showcase, billing not a concern).** Synapse is the reference implementation with the most complete Application Service API and the richest bridge ecosystem — the exact surfaces this project exercises. MAS unlocks **Element X** (the modern mobile client, which hard-requires OIDC + native sliding sync) and enterprise SSO/IdP integration. ESS is _Element's own_ self-host distribution — the most credible "this is how a company would actually run it" reference.
- **Rejected alternatives.**
  1. _Continuwuity / Tuwunel (Rust, ~256 MiB)._ The right pick under a tight RAM/cost budget (that is `dev.fmind`'s world), but weaker Element X / MAS story and 2026 appservice-E2EE edge cases — less convincing as an enterprise reference. Documented as the "budget swap" in the ADR.
  1. _Dendrite (Go)._ Maintenance-mode with an Application Service API officially "not well tested" — wrong foundation for a bridge showcase.
- **Postgres.** ESS is configured to use the **shared CloudNativePG** cluster (external Postgres), demonstrating the "bridging infra, schema/DB per service" pattern rather than a bundled DB. Synapse requires its database created with `C` collation — provisioned explicitly (see §4.5).

### 4.2 Delegation protocol → **A2A (`message/send`, non-streaming), `a2a-go` v2**

- **Decision.** Agents are invoked over **A2A** using the official Go SDK `github.com/a2aproject/a2a-go/v2` (`a2aclient`), **`message/send` only** — a synchronous round trip returning a `Task | Message`. Streaming (`message/stream`, SSE) is deliberately unused. See [ADR 0004](docs/adr/0004-a2a-delegation.md).
- **Why.** A2A is a Linux-Foundation-governed open standard; `message/send` maps exactly onto the fire-and-forget "post a task, get a reply" gesture. It is the **same SDK kagent uses** (DRY, and the `SendMessageResult` sum type is type-safe at the boundary). AgentCard discovery (`/.well-known/agent-card.json`) makes agents self-describing.
- **Rejected.** `trpc-group/trpc-a2a-go` (viable, lower Go floor) — official SDK preferred per "latest-stable + official." Hand-rolled JSON-RPC — reinvents typed transport/card discovery.

### 4.3 The bridge → **one Go appservice on `mautrix/go` (not bridgev2)**

- **Decision.** A single Go binary using the **`mautrix/go` `appservice` package** (`v0.28.1`), NOT the `bridgev2` framework. See [ADR 0005](docs/adr/0005-matrix-a2a-bridge-appservice.md) and [`apps/matrix-a2a-bridge/`](apps/matrix-a2a-bridge/).
- **Why.** `bridgev2` models a _remote network mirrored into portals_ — for native human+agent rooms there is no foreign network to portal, so it would force inventing portals/logins for nothing. The plain `appservice` gives namespace registration, event transport, and multi-ghost puppeting directly. `bridgev2` (and the off-the-shelf mautrix bridges built on it) is reserved for the **external-network interop** phase (§7).
- **Footprint.** A push-based Go appservice idles at ~15–40 MiB — negligible; it runs no per-ghost `/sync`.

### 4.4 Egress governance → **route A2A through agentgateway; agents' LLM egress through agentgateway**

- **Decision.** The bridge's A2A calls route **through agentgateway** (ClusterIP), and each kagent agent's model calls egress **through agentgateway** (OpenAI-compatible endpoint). agentgateway is the single **model-credential chokepoint** with unified LLM/MCP/A2A telemetry. See [ADR 0006](docs/adr/0006-agentgateway-chokepoint.md).
- **Honest scope.** On the _bridge→agent_ hop, agentgateway provides **telemetry + agent-card URL rewriting**, not response inspection or A2A-level authz — so hitting kagent's A2A endpoint directly is functionally equivalent for fire-and-forget. The credential-centralisation benefit lives on the _agent→LLM_ hop. Routing A2A through the gateway is therefore an **observability/governance choice** (valuable for the showcase), toggleable in the bridge config. Who-may-invoke-which-agent authorization lives in the bridge (an allowlist).

### 4.5 Shared state → **CloudNativePG, database-per-service**

- **Decision.** One shared **CloudNativePG** cluster (`platform-pg`, ns `postgres`) exposes dedicated databases/roles: **`synapse`** (collation `C`, Synapse's requirement), **`mas`**, and **`bridge`** (mautrix StateStore, so ghost registrations survive pod restarts). See [ADR 0007](docs/adr/0007-shared-postgres-db-per-service.md). TLS enforced (`sslmode=require`).
- **Why.** Mirrors the "bridging infra, not embedded databases" principle; a single operator, backups, and connection policy for all stateful tenants. (Note: mautrix _bridges_ — the future interop phase — officially want a dedicated database each; that is honored by adding a DB per bridge.)

### 4.6 E2EE → **agent rooms unencrypted, enforced server-side**

- **Decision.** Agent/collaboration rooms are **unencrypted**, force-disabled git-declaratively via `io.element.e2ee.default` / `force_disable` in `/.well-known/matrix/client`. The bridge does **not** wire the crypto package. See [ADR 0008](docs/adr/0008-unencrypted-agent-rooms.md).
- **Why.** A bot/appservice in a room does not force crypto (it simply receives plaintext in unencrypted rooms). Appservice-E2EE is officially "not recommended" by mautrix and is config-heavy; for a private, TLS-terminated, in-cluster deployment, unencrypted agent rooms are a defensible trade-off and sidestep the whole crypto cost cliff. Enterprises that require sovereign E2EE between _humans_ still get it in human-only rooms; the agent-delegation rooms are the ones kept plaintext. This is documented as an explicit, revisitable decision.

---

## 5. Repository Layout

```text
fgentic/
├── PLAN.md                     # this document — the durable architecture + research record
├── README.md                   # human-facing project intro + quickstart
├── LICENSE                     # MIT (public OSS)
├── mise.toml                   # root task vocabulary + pinned infra toolchain
├── skaffold.yaml               # local (k3d/kind) dev loop for the bridge
├── dprint.json · lefthook.yml · trivy.yaml · .gitleaks.toml · .sops.yaml · .gitignore
├── .agents/
│   ├── AGENTS.md               # repo rules, layout, principles (this stack's conventions)
│   └── skills/matrix-agents/   # operator runbooks (bootstrap, add-agent, add-bridge, DNS)
├── docs/adr/                   # Architecture Decision Records (0001-0008)
├── apps/
│   └── matrix-a2a-bridge/      # THE Go bridge: mautrix/go appservice + a2a-go client
│       ├── cmd/bridge · internal/{config,matrixapp,a2aclient,bridge}
│       ├── chart/              # Helm chart (Deployment, Service, NetworkPolicy, …)
│       ├── Dockerfile · mise.toml · registration.example.yaml · agents.example.yaml
├── infra/                      # cluster-wide infrastructure (reconciled by Flux)
│   ├── terraform/              # optional GKE reference cluster (provider-independent workloads)
│   ├── flux/                   # platform Helm layer (HelmRepositories + HelmReleases)
│   ├── gateway/                # Gateway API + Let's Encrypt TLS (chat./matrix./auth.)
│   ├── postgres/               # shared CloudNativePG cluster + databases/roles
│   ├── matrix/                 # ESS (Synapse + MAS + Element + well-known) HelmRelease + values
│   ├── agentgateway/           # AI-native data plane (LLM + A2A routing)
│   ├── kagent/                 # kagent platform + sample Agents (a2aConfig)
│   ├── bridges/                # (phase 4) mautrix-slack / -telegram interop appservices
│   └── secrets/                # SOPS-encrypted secrets (*.sops.yaml) + *.example templates
└── clusters/
    └── platform/               # Flux entrypoint: the Kustomization DAG over infra/ + apps/
```

---

## 6. Delivery & Environments

- **Production CD: Flux v2, pull-based GitOps.** `clusters/platform/` is the Flux entrypoint; Kustomizations reconcile `infra/` + `apps/` in dependency order. Commit to git → Flux reconciles. GitHub Actions is CI-only (build/test/scan/sign the bridge image, push to GHCR, commit the digest).
- **Helm-first manifests.** Platform components are `HelmRepository`/`OCIRepository` + `HelmRelease` with inline values; per-directory `kustomization.yaml` lists resources; `base` + overlays where environments differ.
- **Secrets: SOPS + age.** Real secrets committed only as `*.sops.yaml`; decrypted in-cluster by Flux's kustomize-controller. `*.example` templates document each secret's shape. (Secrets here: ESS registration/signing keys, the appservice `as_token`/`hs_token`, Postgres role passwords, the agentgateway model key or Workload Identity, MAS encryption secrets.)
- **Local development: k3d (or kind) + Skaffold.** The same charts/overlays deploy locally; the bridge has a Skaffold dev loop. Element X requires public HTTPS + MAS, so mobile testing uses a real domain; Element Web works locally over the k3d gateway.
- **Provider independence.** Workloads are plain Kubernetes; only `infra/terraform/` is cloud-specific (an _optional_ GKE reference). The platform runs on any conformant cluster.

---

## 7. Interoperability Roadmap (the "connect WhatsApp/Slack later" promise)

Matrix's bridge ecosystem makes external-network interop a **config addition, not a rebuild** — because a Matrix room is network-agnostic, an agent in a room transparently talks to a bridged WhatsApp/Slack user, and appservices coexist as long as their user-ID namespaces are disjoint.

1. **Reuse off-the-shelf mautrix bridges** (all Go, bridgev2, Beeper-grade): `mautrix-telegram`, `mautrix-signal`, `mautrix-slack`, `mautrix-whatsapp`, `mautrix-discord`. Each is a separate appservice with its own registration + **its own database** (mautrix requires a dedicated DB per bridge — add one to CloudNativePG per bridge).
1. **Order by friction.** Start with clean-ToS networks (Telegram, Signal, Slack); defer WhatsApp/Meta (ToS exposure + a phone kept online). `mautrix-discord` still lags on the legacy architecture.
1. **Namespaces stay disjoint.** Agent ghosts are `@agent-.*`; each bridge claims its own (e.g. `@telegram_.*`), so nothing collides.
1. **Lighter alternative for pure notifications:** `matrix-hookshot` (webhooks, GitHub/RSS/Slack-compatible) for "agents post alerts / receive events," when full puppeting is not needed.

---

## 8. Risks & Limitations (eyes open)

| Risk / limitation                                                                                                        | Severity        | Mitigation                                                                                                                                                         |
| ------------------------------------------------------------------------------------------------------------------------ | --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Homeserver operational weight (Synapse upgrade cadence + out-of-band security releases; media store; key backups)        | Medium          | ESS packages upgrades; Flux image automation + a documented upgrade runbook; federation disabled unless needed to shrink surface.                                  |
| Appservice registration is partly imperative (the homeserver must load `registration.yaml`; adding namespaces = restart) | Medium          | ESS takes the registration as config (git-declared via a Secret); document the token-generation bootstrap step.                                                    |
| `SendMessageResult` is a sum type; extracting text from a `Task` (Status.Message / last artifact) is fiddly              | Low             | One well-tested `SendMessageResult → string` helper in the bridge, unit-tested against a real `a2asrv` fixture.                                                    |
| Slow agents block a synchronous `message/send`                                                                           | Medium          | 60s context deadline; optional "working…" notice; adopt `tasks/get` polling only if long-running agents appear.                                                    |
| "Through agentgateway" gives telemetry, not authz, on the bridge→agent hop                                               | Low             | Who-can-invoke-which-agent allowlist lives in the bridge; agentgateway remains the LLM-credential chokepoint.                                                      |
| E2EE off for agent rooms                                                                                                 | Medium (policy) | Server-side force-disable is explicit and documented; human-only rooms may still be encrypted; revisit if sovereign E2EE across agent rooms becomes a requirement. |
| External-network bridges are reverse-engineered (break on upstream change; WhatsApp/Meta ToS)                            | Low–Med         | Add incrementally on real demand; prefer clean-ToS networks; each is isolated as its own appservice.                                                               |
| Element X drives homeserver requirements (MAS + native sliding sync)                                                     | Low             | ESS provides both; Element Web is the always-available fallback needing neither.                                                                                   |

---

## 9. Phased Build Roadmap

1. **Phase 0 — Foundations & tooling.** Repo scaffold, `mise` vocabulary, dprint/lefthook/trivy/gitleaks, SOPS config, Flux entrypoint, Gateway API + cert-manager, CloudNativePG. _(This commit.)_
1. **Phase 1 — Matrix layer.** ESS (Synapse + MAS + Element Web + well-known) HelmRelease against shared Postgres; DNS + TLS for `matrix./auth./chat.fgentic.fmind.ai` and apex `.well-known` delegation; provision a human account + verify Element Web login and a plain room.
1. **Phase 2 — Agent layer.** agentgateway (LLM egress chokepoint + A2A route to kagent) and kagent with sample Agents (`a2aConfig`) exposed as A2A servers; verify `a2a discover` / `a2a send` against an agent from the shell.
1. **Phase 3 — The bridge.** Deploy `matrix-a2a-bridge` (appservice registered with Synapse via ESS config); create `@agent-k8s`/`@agent-helm` ghosts; verify end-to-end `@mention → A2A → reply` in a room. Back the StateStore with CloudNativePG.
1. **Phase 4 — Interoperability (on demand).** Add one off-the-shelf mautrix bridge (Telegram/Signal/Slack) as a separate appservice + its own database; demonstrate an agent talking to a bridged user; enable Element X (mobile) via MAS.
1. **Phase 5 — Hardening & docs.** NetworkPolicies, RBAC scoping, resource tuning, dashboards, the operator runbooks, and an end-to-end demo script for the "enterprise showcase" narrative.

---

## 10. Sources (verified 2026-07-08)

- Matrix: <https://matrix.org>, <https://spec.matrix.org>, ESS <https://github.com/element-hq/ess-helm>, Synapse <https://github.com/element-hq/synapse>, MAS <https://github.com/element-hq/matrix-authentication-service>, Element <https://github.com/element-hq/element-web>, Element X <https://github.com/element-hq/element-x-ios> / `-android`.
- A2A: spec <https://a2a-protocol.org/v0.3.0/specification/>, <https://github.com/a2aproject/A2A>, Go SDK <https://github.com/a2aproject/a2a-go> (`/v2`).
- mautrix/go: <https://github.com/mautrix/go> (`v0.28.1`, import `maunium.net/go/mautrix`); bridges <https://docs.mau.fi>, <https://github.com/mautrix>.
- kagent: <https://kagent.dev>, <https://github.com/kagent-dev/kagent> (A2A on controller `:8083`, AgentCard at `/.well-known/agent-card.json`).
- agentgateway: <https://agentgateway.dev>, <https://github.com/agentgateway/agentgateway> (A2A backend + routing, v1.0.0 2026-03).
- Platform: CloudNativePG <https://cloudnative-pg.io>, Gateway API <https://gateway-api.sigs.k8s.io>, cert-manager <https://cert-manager.io>, Flux <https://fluxcd.io>, SOPS <https://github.com/getsops/sops>.

> Full research detail (homeserver footprint comparison, the adversarial verification of each hop, prior-art survey, and the "why not a simpler alternative" analysis) informed this plan and is summarised in the ADRs. The decisive difference from `dev.fmind`: this project chooses the **enterprise-credible** option at every fork (Synapse+MAS over a lightweight Rust server; full A2A-through-agentgateway governance; database-per-service) because its goal is to demonstrate the pattern, not to fit a $30/month node.
