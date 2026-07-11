# Fgentic — Sovereign Agentic Collaboration Platform

**Humans and AI agents in the same chat rooms. `@mention` an agent to delegate a task. It replies. Self-hosted, out of the box, on open standards — you choose where your prompts go, and organizations can ultimately federate so agents collaborate across company boundaries.**

The agentic AI landscape is consolidating around closed, tenant-anchored platforms — Slack, Teams, and Google Chat all ship @mentionable agents, every one anchored to a vendor's tenant. **Fgentic** is the open counter-proposal: a **sovereign** platform an enterprise can self-host end-to-end — built exclusively from open protocols and genuinely open-source components, with every layer swappable, a per-cluster choice of model backend (self-hosted included), no proprietary SaaS in the critical path, and **federation** as the destination. It stitches together five open pieces:

- **[Matrix](https://matrix.org)** — the collaboration fabric and UI (self-hosted **Synapse** + **Matrix Authentication Service**, with **Element** Web/X as clients). Matrix federation is the only cross-organization messaging federation proven in production at scale (Germany's healthcare system, NATO, Sweden's public sector).
- **[A2A](https://a2a-protocol.org)** (Agent2Agent, a Linux Foundation protocol, spec v1.0) — how tasks are delegated to agents, within and across organizations.
- **[kagent](https://kagent.dev)** (CNCF) — AI agents running natively on Kubernetes, each exposed as an A2A server.
- **[agentgateway](https://agentgateway.dev)** (Agentic AI Foundation) — the AI-native data plane: one governed chokepoint for all LLM/MCP/A2A traffic, where the only model credential lives.
- **A small Go bridge** — [`matrix-a2a-bridge`](apps/matrix-a2a-bridge/) — the novel glue that turns an `@mention` into an A2A call and posts the reply back. (As far as we can find, the first Matrix↔A2A appservice bridge.)

Because Matrix is a bridged protocol, the same rooms can also connect to **Slack, WhatsApp, Signal, Telegram** and more — "chat with my agents" becomes "chat with my agents _and_ anyone, on any network."

> **New here?** The specification lives under [docs/](docs/), split by topic: [architecture & vision](docs/architecture.md), [design decisions D1–D16](docs/design-decisions.md), [bridge behavior](docs/bridge.md), [security model](docs/security.md), [federation design](docs/federation.md), [observability](docs/observability.md), [licensing strategy](docs/licensing.md), [roadmap history](docs/roadmap.md) — plus the [ADRs](docs/adr/). The executable roadmap is the set of [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones).

---

## Core principles

1. **Sovereignty first.** Self-hosted on any conformant Kubernetes; the model backend is a per-cluster choice ranked by sovereignty — self-hosted vLLM (prompts never leave the cluster) > EU API (Mistral) > hyperscalers (Vertex/Anthropic/OpenAI) — and every layer has a documented exit ([docs/design-decisions.md](docs/design-decisions.md)).
1. **Open standards only.** Matrix, A2A, MCP, OIDC, Kubernetes, Gateway API. Every component is replaceable; the protocols are the platform.
1. **No strings attached.** No paywalls or feature-gated open-core in the critical path — and where an upstream component has caveats (ESS Community's Pro-gated HA, the AGPL Matrix/observability components), we say so openly and document Apache-2.0-licensed alternatives ([docs/licensing.md](docs/licensing.md)).
1. **Federation is the destination.** One sovereign org is the on-ramp; the endgame is companies collaborating agent-to-agent across boundaries — Matrix federation for the shared-room collaboration plane, A2A (Signed AgentCards, mTLS/OIDC) for the direct delegation plane ([docs/federation.md](docs/federation.md), milestone M8).
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

Data-flow details and async/long-task behavior: [docs/bridge.md](docs/bridge.md); the layer map: [docs/architecture.md](docs/architecture.md).

## Architecture at a glance

| Layer                      | Component                                                                                                   | Namespace             |
| -------------------------- | ----------------------------------------------------------------------------------------------------------- | --------------------- |
| UI + collaboration fabric  | Element Web/X · Synapse · MAS (via [ESS Community](https://github.com/element-hq/ess-helm))                 | `matrix`              |
| The bridge (the glue)      | `matrix-a2a-bridge` (Go, `mautrix/go` appservice + `a2a-go`)                                                | `bridge`              |
| AI data plane / governance | agentgateway (LLM + A2A routing, credential chokepoint)                                                     | `agentgateway-system` |
| Agents                     | kagent (Agent CRDs served as A2A on `:8083`)                                                                | `kagent`              |
| Shared state               | CloudNativePG (databases: `synapse`, `mas`, `bridge`, `kagent`)                                             | `postgres`            |
| Web ingress + TLS          | Gateway API (Traefik) + cert-manager (Let's Encrypt)                                                        | `gateway`             |
| Observability              | kube-prometheus-stack: Prometheus · Grafana · Alertmanager ([docs/observability.md](docs/observability.md)) | `monitoring`          |
| Delivery                   | Flux v2 pull-based GitOps                                                                                   | `flux-system`         |

Reference deployment: `fgentic.fmind.ai` (Element at `chat.`, Synapse at `matrix.`, MAS at `auth.`; user IDs `@name:fgentic.fmind.ai` via apex `.well-known` delegation).

## Key decisions (the short version — details in [docs/design-decisions.md](docs/design-decisions.md))

1. **Homeserver: Synapse + MAS + Element via ESS Community, deliberately.** We evaluated the whole 2026 homeserver landscape (Tuwunel, continuwuity, Conduit, chart-less Synapse, Palpo) and stayed: ESS wins on everything this platform lives on — appservice API maturity, modern auth (MAS) + Element X, federation policy hooks, and PostgreSQL. ESS Community is AGPL and open-core (Element gates HA, LDAP, and its federation border gateway behind ESS Pro) — we say that openly, keep a documented **Apache-2.0 fallback profile** (Tuwunel/continuwuity) for AGPL-averse deployments, and define concrete triggers for ever switching ([docs/licensing.md](docs/licensing.md)).
1. **License: Apache-2.0, DCO, no CLA.** Patent grant, foundation-donation fit (CNCF/AAIF), coherence with A2A/MCP/kagent/agentgateway. The bridge embeds `mautrix/go` (MPL-2.0) — attribution ships in [NOTICE](NOTICE).
1. **Federation honesty.** Matrix federation gives _organization-level_ cryptographic identity (a partner's server vouches for its users), full room replication to every participating server, and best-effort cross-server redaction. We design for that reality — closed federation, room v12, server ACLs, per-agent sender allowlists, policy-as-code borders, and A2A v1.0 Signed AgentCards for per-agent identity — instead of overclaiming ([docs/federation.md](docs/federation.md)).
1. **Cost is a first-class failure mode.** Every mention is an LLM invocation, so the bridge rate-limits per sender and per room, and agentgateway meters tokens and spend — the closest prior-art project died of exactly this.

## Project status & roadmap

**Live end-to-end on the local reference cluster.** A Matrix `@mention` in Element produces a real LLM-backed agent reply — through the bridge, agentgateway (no agent ever holds a model key), and kagent, with conversation threading, rate limits, sanitized failure replies, and Prometheus/Grafana observability (bridge delegation metrics + gateway GenAI token metering + the LLM spend alert). Every layer reconciles from this repository via Flux; the same manifests drive the GKE reference profile (`clusters/gcp`). The adversarial-review fixes (D1–D15) are all implemented and unit-tested ([docs/design-decisions.md](docs/design-decisions.md)).

**The roadmap lives on [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones)** (M0–M11, each with an epic tracker issue), sequenced sovereignty-first: hygiene → sovereign model profiles → enterprise SSO → one-command evaluation install → test harness → traces & audit → security hardening → interop bridges (Slack first) → **federation preview (the thesis)** → production reference → community → the sovereignty kit (reference architecture + compliance + exit strategy). Issues labeled `agent-ready` are groomed for autonomous coding agents; `needs-human` marks decisions, approvals, or spend ([docs/roadmap.md](docs/roadmap.md)).

## Quickstart (local, k3d)

Prerequisites: Docker, [k3d](https://k3d.io/), `kubectl`, [flux](https://fluxcd.io/), [mise](https://mise.jdx.dev/), [sops](https://github.com/getsops/sops) + an age key (`age-keygen`; recipient in [.sops.yaml](.sops.yaml)).

```bash
mise install                          # git hooks + pinned toolchain
mise run cluster:up                   # k3d cluster on loopback 80/443 (*.fgentic.localhost)
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/experimental-install.yaml
scripts/gen-secrets.sh fgentic.localhost local   # SOPS secret set -> commit + push
kubectl create ns flux-system && kubectl -n flux-system create secret generic sops-age   --from-file=age.agekey="$HOME/.config/sops/age/keys.txt"
scripts/local-ca.sh                   # local TLS CA (ESS bakes https URLs)
scripts/local-adc.sh <gcp-project>    # Vertex AI credentials for agentgateway (or swap the provider)
flux bootstrap github --owner=<you> --repository=fgentic --path=clusters/local
mise run bridge:load                  # build + side-load the bridge image
```

Flux reconciles the whole platform. Then open `https://chat.fgentic.localhost` (Element Web), sign in (create a user with `mas-cli manage register-user` in the MAS pod), create a room, invite `@agent-assistant:fgentic.localhost`, and `@mention` it. Grafana lives at `https://grafana.fgentic.localhost`. Full runbook: [matrix-agents](.agents/skills/matrix-agents/SKILL.md).

## Production (Kubernetes, any provider)

Production reconciles itself from git via **Flux v2** (`clusters/<cluster>/` overlays over `clusters/base/` are the entrypoints). An optional GKE reference cluster lives in [`infra/terraform/`](infra/terraform/); the workloads are plain Kubernetes and run on any conformant cluster. Hardening and production profile: [docs/architecture.md](docs/architecture.md) + [docs/security.md](docs/security.md).

## Repository layout

```text
apps/matrix-a2a-bridge/  # the Go bridge (mautrix/go appservice + a2a-go client) + its deploy/ Flux unit
infra/{terraform,flux,gateway,postgres,matrix,agentgateway,kagent,bridges,secrets}
clusters/               # Flux entrypoints: base/ DAG + local/ (k3d) and gcp/ (GKE) overlays
docs/                    # the specification split by topic (architecture, decisions, security, federation, …) + docs/adr/
.github/                 # CI (mise gates) + CD (signed, digest-pinned bridge image) + issue/PR templates
.agents/                 # AGENTS.md + operator runbooks
CONTRIBUTING.md          # how to contribute (workflow, labels, DCO) — with GOVERNANCE, SECURITY, MAINTAINERS, ADOPTERS
```

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for the workflow (issues, labels, DCO sign-off) and [GOVERNANCE.md](GOVERNANCE.md) for how the project is run. The short version:

1. Start with [docs/](docs/) — the [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones) are the backlog; [design decisions](docs/design-decisions.md) and ADRs in [docs/adr/](docs/adr/) capture what is settled (propose a new ADR to revisit one).
1. `mise run check` and `mise run test` must pass warning-free; git hooks (lefthook) enforce the same tasks locally that CI runs.
1. Conventions live in [.agents/AGENTS.md](.agents/AGENTS.md) (they bind human and AI contributors alike): Go, type-safe, small composable units, no tech debt, Conventional Commits, DCO sign-off.
1. Never commit plaintext secrets — SOPS-encrypted `*.sops.yaml` only (gitleaks runs pre-commit).

Security reports go through [SECURITY.md](SECURITY.md), not public issues. Deployments and pilots: add yourself to [ADOPTERS.md](ADOPTERS.md).

## Standards & building blocks

Matrix (spec.matrix.org) · A2A (a2a-protocol.org, Linux Foundation) · MCP (Agentic AI Foundation) · Kubernetes · Gateway API · OpenID Connect (via MAS) · SOPS/age. Everything is open source and self-hostable.

## License

[Apache-2.0](LICENSE) © Médéric Hurier (Fmind) — chosen for its explicit patent grant, foundation-donation requirements (CNCF/AAIF), and coherence with the A2A/MCP/kagent/agentgateway stack ([docs/licensing.md](docs/licensing.md)). The bridge embeds `mautrix/go` (MPL-2.0) — the third-party [NOTICE](NOTICE) ships with the binary image. Contributions use DCO sign-off.
