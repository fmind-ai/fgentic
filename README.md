# Fgentic — Federated Agentic Collaboration Platform

**Humans and AI agents in the same chat rooms. `@mention` an agent to delegate a task. It replies. Organizations federate, so agents collaborate across company boundaries. All on open standards, all self-hosted, all on Kubernetes.**

The agentic AI landscape is consolidating around closed, tenant-anchored platforms. **Fgentic** is the open counter-proposal: a reference platform an enterprise can self-host and **federate** — built exclusively from open protocols and genuinely open-source components, with every layer swappable and no proprietary SaaS in the critical path. It stitches together five open pieces:

- **[Matrix](https://matrix.org)** — the collaboration fabric and UI (self-hosted **Synapse** + **Matrix Authentication Service**, with **Element** Web/X as clients). Matrix federation is the only cross-organization messaging federation proven in production at scale (Germany's healthcare system, NATO, Sweden's public sector).
- **[A2A](https://a2a-protocol.org)** (Agent2Agent, a Linux Foundation protocol, spec v1.0) — how tasks are delegated to agents, within and across organizations.
- **[kagent](https://kagent.dev)** (CNCF) — AI agents running natively on Kubernetes, each exposed as an A2A server.
- **[agentgateway](https://agentgateway.dev)** (Agentic AI Foundation) — the AI-native data plane: one governed chokepoint for all LLM/MCP/A2A traffic, where the only model credential lives.
- **A small Go bridge** — [`matrix-a2a-bridge`](apps/matrix-a2a-bridge/) — the novel glue that turns an `@mention` into an A2A call and posts the reply back. (As far as we can find, the first Matrix↔A2A appservice bridge.)

Because Matrix is a bridged protocol, the same rooms can also connect to **Slack, WhatsApp, Signal, Telegram** and more — "chat with my agents" becomes "chat with my agents _and_ anyone, on any network."

> **New here?** [SPEC.md](SPEC.md) is the binding technical specification and plan — architecture, design decisions with evidence, security model, federation design, roadmap. [PLAN.md](PLAN.md) is the original research record. Read SPEC for the _what and how_, PLAN for the _origin story_.

---

## Core principles

1. **Open standards only.** Matrix, A2A, MCP, OIDC, Kubernetes, Gateway API. Every component is replaceable; the protocols are the platform.
1. **No strings attached.** No paywalls or feature-gated open-core in the critical path — and where an upstream component has caveats (ESS Community's Pro-gated HA, the AGPL Matrix/observability components), we say so openly and document Apache-2.0-licensed alternatives ([SPEC.md §10](SPEC.md)).
1. **Federation is the point.** One org is the on-ramp; the destination is companies collaborating agent-to-agent across boundaries — Matrix federation for the shared-room collaboration plane, A2A (Signed AgentCards, mTLS/OIDC) for the direct delegation plane.
1. **Governed by construction.** All agent LLM egress flows through agentgateway (single credential, unified telemetry, token budgets); every agent invocation is attributable to a Matrix identity.
1. **GitOps everything.** Flux v2 reconciles the entire platform from this repository; secrets are SOPS-encrypted; nothing is applied by hand.

## The core interaction

```text
Human in Element:  "@agent-k8s why is pod payments-7c9 crashing?"
        │  (Matrix Client-Server API)
        ▼
   Synapse ──push──▶ matrix-a2a-bridge (Go appservice)
                          │  detect @mention → map @agent-k8s → (kagent, k8s-agent)
                          │  A2A message/send ── through agentgateway ──▶ kagent agent
                          ◀── reply text ─────────────────────────────────┘
                          │  post as ghost @agent-k8s (reply to the original message)
        ▼
Human in Element:  "@agent-k8s: The container is OOMKilled — memory limit 128Mi …"
```

Full step-by-step flow: [PLAN.md §3.1](PLAN.md#31-the-mention--a2a--reply-data-flow-step-by-step). Async/long-task behavior: [SPEC.md §6](SPEC.md).

## Architecture at a glance

| Layer                      | Component                                                                                   | Namespace             |
| -------------------------- | ------------------------------------------------------------------------------------------- | --------------------- |
| UI + collaboration fabric  | Element Web/X · Synapse · MAS (via [ESS Community](https://github.com/element-hq/ess-helm)) | `matrix`              |
| The bridge (the glue)      | `matrix-a2a-bridge` (Go, `mautrix/go` appservice + `a2a-go`)                                | `bridge`              |
| AI data plane / governance | agentgateway (LLM + A2A routing, credential chokepoint)                                     | `agentgateway-system` |
| Agents                     | kagent (Agent CRDs served as A2A on `:8083`)                                                | `kagent`              |
| Shared state               | CloudNativePG (databases: `synapse`, `mas`, `bridge`, `kagent`)                             | `postgres`            |
| Web ingress + TLS          | Gateway API (Traefik) + cert-manager (Let's Encrypt)                                        | `gateway`             |
| Observability              | Prometheus · Grafana · OTel · MLflow ([SPEC.md §9](SPEC.md))                                | `monitoring`          |
| Delivery                   | Flux v2 pull-based GitOps                                                                   | `flux-system`         |

Reference deployment: `fgentic.fmind.ai` (Element at `chat.`, Synapse at `matrix.`, MAS at `auth.`; user IDs `@name:fgentic.fmind.ai` via apex `.well-known` delegation).

## Key decisions (the short version — details in SPEC.md)

1. **Homeserver: Synapse + MAS + Element via ESS Community, deliberately.** We evaluated the whole 2026 homeserver landscape (Tuwunel, continuwuity, Conduit, chart-less Synapse, Palpo) and stayed: ESS wins on everything this platform lives on — appservice API maturity, modern auth (MAS) + Element X, federation policy hooks, and PostgreSQL. ESS Community is AGPL and open-core (Element gates HA, LDAP, and its federation border gateway behind ESS Pro) — we say that openly, keep a documented **Apache-2.0 fallback profile** (Tuwunel/continuwuity) for AGPL-averse deployments, and define concrete triggers for ever switching ([SPEC.md §10.3](SPEC.md)).
1. **License: Apache-2.0, DCO, no CLA.** Patent grant, foundation-donation fit (CNCF/AAIF), coherence with A2A/MCP/kagent/agentgateway. The bridge embeds `mautrix/go` (MPL-2.0) — attribution ships in [NOTICE](NOTICE).
1. **Federation honesty.** Matrix federation gives _organization-level_ cryptographic identity (a partner's server vouches for its users), full room replication to every participating server, and best-effort cross-server redaction. We design for that reality — closed federation, room v12, server ACLs, per-agent sender allowlists, policy-as-code borders, and A2A v1.0 Signed AgentCards for per-agent identity — instead of overclaiming ([SPEC.md §8](SPEC.md)).
1. **Cost is a first-class failure mode.** Every mention is an LLM invocation, so the bridge rate-limits per sender and per room, and agentgateway meters tokens and spend — the closest prior-art project died of exactly this.

## Project status

**Foundation built and reviewed — not yet deployed.** The adversarial architecture review is complete and every finding is fixed in code: async bounded dispatch, Postgres-backed state, federation-safe mention resolution, sender allowlists, rate limits, long-task polling with message edits, database backups, and CI/CD with signed, digest-pinned images ([SPEC.md §4](SPEC.md)). The bridge passes its race-enabled unit suite and all static gates; the first end-to-end deployment is Phase 1 of the [roadmap](SPEC.md). Contributions are welcome precisely at this stage — the design conversation is still open.

## Quickstart (local, k3d)

Prerequisites: Docker, [k3d](https://k3d.io/), `kubectl`, [helm](https://helm.sh/), [skaffold](https://skaffold.dev/), [mise](https://mise.jdx.dev/), [sops](https://github.com/getsops/sops) + an age key.

```bash
mise install                 # git hooks + pinned toolchain
mise run cluster:up          # create the local k3d cluster
# apply the platform (Gateway API + cert-manager + CloudNativePG + ESS + agentgateway + kagent)
# then run the bridge dev loop:
mise run watch               # skaffold dev — builds & live-syncs matrix-a2a-bridge
```

Open Element Web at the local gateway, create a room, invite `@agent-assistant`, and `@mention` it. See the [matrix-agents runbook](.agents/skills/matrix-agents/SKILL.md) for the full bootstrap (accounts, appservice registration, DNS/TLS).

## Production (Kubernetes, any provider)

Production reconciles itself from git via **Flux v2** (`clusters/<cluster>/` overlays over `clusters/base/` are the entrypoints). An optional GKE reference cluster lives in [`infra/terraform/`](infra/terraform/); the workloads are plain Kubernetes and run on any conformant cluster. Delivery model: [PLAN.md §6](PLAN.md); hardening and production profile: [SPEC.md](SPEC.md).

## Repository layout

```text
apps/matrix-a2a-bridge/  # the Go bridge (mautrix/go appservice + a2a-go client) + its deploy/ Flux unit
infra/{terraform,flux,gateway,postgres,matrix,agentgateway,kagent,bridges,secrets}
clusters/               # Flux entrypoints: base/ DAG + local/ (k3d) and gcp/ (GKE) overlays
docs/adr/                # Architecture Decision Records
.github/workflows/       # CI (mise gates) + CD (signed, digest-pinned bridge image)
.agents/                 # AGENTS.md + operator runbooks
SPEC.md                  # the binding technical specification (decisions, security, federation, roadmap)
PLAN.md                  # the original architecture + research record
```

## Contributing

1. Start with [SPEC.md](SPEC.md) — the roadmap (§13) is the backlog; design decisions (§4) and ADRs in [docs/adr/](docs/adr/) capture what is settled (propose a new ADR to revisit one).
1. `mise run check` and `mise run test` must pass warning-free; git hooks (lefthook) enforce the same tasks locally that CI runs.
1. Conventions live in [.agents/AGENTS.md](.agents/AGENTS.md) (they bind human and AI contributors alike): Go, type-safe, small composable units, no tech debt, Conventional Commits, DCO sign-off.
1. Never commit plaintext secrets — SOPS-encrypted `*.sops.yaml` only (gitleaks runs pre-commit).

## Standards & building blocks

Matrix (spec.matrix.org) · A2A (a2a-protocol.org, Linux Foundation) · MCP (Agentic AI Foundation) · Kubernetes · Gateway API · OpenID Connect (via MAS) · SOPS/age. Everything is open source and self-hostable.

## License

[Apache-2.0](LICENSE) © Médéric Hurier (Fmind) — chosen for its explicit patent grant, foundation-donation requirements (CNCF/AAIF), and coherence with the A2A/MCP/kagent/agentgateway stack ([SPEC.md §10](SPEC.md)). The bridge embeds `mautrix/go` (MPL-2.0) — the third-party [NOTICE](NOTICE) ships with the binary image. Contributions use DCO sign-off.
