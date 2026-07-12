# Fgentic — Sovereign Agentic Collaboration Platform

**Humans and AI agents in the same chat rooms. `@mention` an agent to delegate a task. It replies. Self-hosted, out of the box, on open standards — you choose where your prompts go, and organizations can ultimately federate so agents collaborate across company boundaries.**

The agentic AI landscape is consolidating around closed, tenant-anchored platforms — Slack, Teams, and Google Chat all ship @mentionable agents, every one anchored to a vendor's tenant. **Fgentic** is the open counter-proposal: a **sovereign** platform an enterprise can self-host end-to-end — built exclusively from open protocols and genuinely open-source components, with every layer swappable, a per-cluster choice of model backend (self-hosted included), no proprietary SaaS in the critical path, and **federation** as the destination. It stitches together five open pieces:

- **[Matrix](https://matrix.org)** — the collaboration fabric and UI (self-hosted **Synapse** + **Matrix Authentication Service**, with **Element** Web/X as clients). Matrix federation is the only cross-organization messaging federation proven in production at scale (Germany's healthcare system, NATO, Sweden's public sector).
- **[A2A](https://a2a-protocol.org)** (Agent2Agent, a Linux Foundation protocol, spec v1.0) — how tasks are delegated to agents, within and across organizations.
- **[kagent](https://kagent.dev)** (CNCF) — AI agents running natively on Kubernetes, each exposed as an A2A server.
- **[agentgateway](https://agentgateway.dev)** (Agentic AI Foundation) — the AI-native data plane: one governed chokepoint for locally hosted LLM/MCP/A2A traffic, where the platform's model credential lives.
- **A small Go bridge** — [`matrix-a2a-bridge`](apps/matrix-a2a-bridge/) — the novel glue that turns an `@mention` into either a local kagent call or an explicitly pinned remote A2A call, then posts the reply back. (As far as we can find, the first Matrix↔A2A appservice bridge.)

Agent-runtime swappability is continuously tested rather than assumed: the bridge integration gate completes the real Matrix appservice round trip against a standalone plain `a2a-go` server while installing no kagent resources.

Fgentic ships opt-in, digest-pinned GitOps units for **Slack** and **Telegram** on top of Matrix's upstream bridge ecosystem. They are disabled by default and require provider-owner, policy, and live bidirectional acceptance; rendered manifests are not a compatibility claim. Signal and WhatsApp remain evaluated candidates, while Microsoft Teams is explicitly [coexistence under review](docs/adr/0011-teams-coexistence-not-bridge.md), not a promised Teams↔Matrix bridge.

> **New here?** The specification lives under [docs/](docs/), split by topic: [architecture & vision](docs/architecture.md), [design decisions D1–D16](docs/design-decisions.md), [identity and SSO](docs/identity.md), [model provider profiles](docs/models.md), [bridge behavior](docs/bridge.md), [external-network interop](docs/interop.md), [security model](docs/security.md), [federation design](docs/federation.md), [partner federation onboarding](docs/federation-onboarding.md), [observability](docs/observability.md), [licensing strategy](docs/licensing.md), [roadmap history](docs/roadmap.md) — plus the [ADRs](docs/adr/). The executable roadmap is the set of [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones).

---

## Core principles

1. **Sovereignty first.** Self-hosted on any conformant Kubernetes; the model backend is a per-cluster choice ranked by sovereignty — self-hosted vLLM (prompts never leave the cluster) > EU API (Mistral) > hyperscalers (Vertex/Anthropic/OpenAI) — and every layer has a documented exit ([docs/design-decisions.md](docs/design-decisions.md)).
1. **Open standards only.** Matrix, A2A, MCP, OIDC, Kubernetes, Gateway API. Every component is replaceable; the protocols are the platform.
1. **No strings attached.** No paywalls or feature-gated open-core in the critical path — and where an upstream component has caveats (ESS Community's Pro-gated HA, the AGPL Matrix/observability components), we say so openly and document Apache-2.0-licensed alternatives ([docs/licensing.md](docs/licensing.md)).
1. **Federation is the destination.** One sovereign org is the on-ramp; the endgame is companies collaborating agent-to-agent across boundaries — Matrix federation for the shared-room collaboration plane, A2A Signed AgentCards plus OIDC JWT or mTLS for the direct delegation plane. The provider-free lab implements OIDC client credentials; mTLS remains the documented alternative ([docs/federation.md](docs/federation.md), milestone M8).
1. **Governed by construction.** Locally hosted agent LLM egress flows through agentgateway (single credential, unified telemetry, token budgets); local room invocations retain their Matrix attribution boundary. A cross-org A2A call is admitted under its verified machine-client identity and reservation quota, while downstream actual-token metrics remain aggregate. An explicitly pinned remote A2A agent is an external trust boundary whose model egress and enforcement belong to the partner.
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
| Optional network interop   | Digest-pinned mautrix Slack/Telegram appservices; disabled until selected and accepted                      | `bridges`             |
| Optional reference IdP     | Keycloak 26.7 via the KeycloakX chart                                                                       | `keycloak`            |
| Optional self-hosted model | vLLM CPU + pinned Qwen2.5-0.5B demo model                                                                   | `models`              |
| Shared state               | CloudNativePG: one database per core service and per enabled external bridge                                | `postgres`            |
| Web ingress + TLS          | Gateway API (Traefik) + cert-manager (Let's Encrypt)                                                        | `gateway`             |
| Observability              | kube-prometheus-stack: Prometheus · Grafana · Alertmanager ([docs/observability.md](docs/observability.md)) | `monitoring`          |
| Delivery                   | Flux v2 pull-based GitOps                                                                                   | `flux-system`         |

Reference deployment: `fgentic.fmind.ai` (Element at `chat.`, Synapse at `matrix.`, MAS at `auth.`, optional Keycloak IdP at `id.`; user IDs `@name:fgentic.fmind.ai` via apex `.well-known` delegation).

## Key decisions (the short version — details in [docs/design-decisions.md](docs/design-decisions.md))

1. **Homeserver: Synapse + MAS + Element via ESS Community, deliberately.** We evaluated the whole 2026 homeserver landscape (Tuwunel, continuwuity, Conduit, chart-less Synapse, Palpo) and stayed: ESS wins on everything this platform lives on — appservice API maturity, modern auth (MAS) + Element X, federation policy hooks, and PostgreSQL. ESS Community is AGPL and open-core (Element gates HA, LDAP, and its federation border gateway behind ESS Pro) — we say that openly, keep a documented **Apache-2.0 fallback profile** (Tuwunel/continuwuity) for AGPL-averse deployments, and define concrete triggers for ever switching ([docs/licensing.md](docs/licensing.md)).
1. **License: Apache-2.0, DCO, no CLA.** Patent grant, foundation-donation fit (CNCF/AAIF), coherence with A2A/MCP/kagent/agentgateway. The bridge embeds `mautrix/go` (MPL-2.0) — attribution ships in [NOTICE](NOTICE).
1. **Federation honesty.** Matrix federation gives _organization-level_ cryptographic identity (a partner's server vouches for its users), full room replication to every participating server, and best-effort cross-server redaction. We design for that reality — closed federation, room v12, server ACLs, per-agent sender allowlists, policy-as-code borders, and A2A v1.0 Signed AgentCards for per-agent identity — instead of overclaiming ([docs/federation.md](docs/federation.md)).
1. **Cost is a first-class failure mode.** Every mention is an LLM invocation, so the bridge rate-limits per sender and per room, and agentgateway meters tokens and spend — the closest prior-art project died of exactly this.

## Project status & roadmap

**Live end-to-end on the local reference cluster.** A Matrix `@mention` in Element produces a real LLM-backed agent reply — through the bridge, agentgateway (no agent ever holds a model key), and kagent, with conversation threading, rate limits, sanitized failure replies, and Prometheus/Grafana observability (bridge delegation metrics + gateway GenAI token metering + the LLM spend alert). The provider-free bridge fixture also proves an explicitly pinned remote URL round-trips only with a valid A2A v1.0 Signed AgentCard and then fails closed after post-signature tampering; no production remote is enabled by default. The opt-in federation lab proves the inbound complement: org B obtains a client-credentials JWT, invokes only org A's signed docs-qa route under an `azp`-scoped token reservation, and reaches the deterministic model while direct kagent remains unpublished. Every layer reconciles from this repository via Flux; the same manifests drive the GKE reference profile (`clusters/gcp`). The adversarial-review fixes (D1–D15) are all implemented and unit-tested ([docs/design-decisions.md](docs/design-decisions.md)).

**The roadmap lives on [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones)** (M0–M17, each with an epic tracker issue), sequenced sovereignty-first: hygiene → sovereign model profiles → enterprise SSO → one-command evaluation install → test harness → traces & audit → security hardening → interop bridges (Slack first) → **federation preview (the thesis)** → production reference → community → the sovereignty kit (reference architecture + compliance + exit strategy) → in-room collaboration and governance UX → rich interaction surfaces → self-service agents & skills → enterprise operations → agent economy & cross-org discovery → personal agent sandboxes. Issues labeled `agent-ready` are groomed for autonomous coding agents; `needs-human` marks decisions, approvals, or spend ([docs/roadmap.md](docs/roadmap.md)).

## Evaluate in 15 minutes

Prerequisites: Docker, Git, and [mise](https://mise.jdx.dev/). The default is deliberately free and deterministic: it exercises Matrix → bridge → agentgateway → kagent with an in-cluster OpenAI-compatible response stub. It proves the integration path, not model quality.

```bash
mise install
mise run demo:up
```

The final output is the Element URL, `@alice:fgentic.localhost`, its generated password, and the seeded `#lobby:fgentic.localhost` room. The mapped ghosts are already members and the welcome mention has received a reply. The command does not mutate the checkout, commit, push, or need a GitHub account; its random credentials live only in the `fgentic-demo` cluster. Set `FGENTIC_DEMO_CACHE_DIR` to a persistent directory to reuse BuildKit layers across repeated installs. Remove only that evaluation cluster with `mise run demo:down`.

Choose the model boundary before using non-demo data:

| Choice                        | Sovereignty and cost boundary                                                                                   |
| ----------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `demo` (default)              | Deterministic cluster-only stub; no model credential, prompt egress, or token charge; not a real language model |
| `vllm`                        | Real self-hosted model; strongest sovereignty, but roughly 2.7 GB of downloads and 4–6 GiB RAM                  |
| `mistral`                     | EU-hosted API path; prompts leave the cluster and the selected account is billed                                |
| `vertex`/`anthropic`/`openai` | Hyperscaler API path; residency and billing depend on the selected account/profile                              |
| `azure-openai`                | Azure deployment boundary; region/data-zone selection and billing remain account controls                       |

For example, `FGENTIC_LLM_PROVIDER=vllm mise run demo:up` selects the real credential-free self-hosted profile. API profiles require the matching key, `FGENTIC_LLM_MODEL`, and `FGENTIC_ALLOW_PAID_PROVIDER=yes`; see the complete [provider contract](docs/models.md). Do not use evaluation credentials or the deterministic stub in production.

To exercise the federation thesis without an external model or provider account, run `mise run fed:up`. It creates a separate `fgentic-fed` cluster with participating Synapse homeservers at `org-a.fgentic.localhost` and `org-b.fgentic.localhost`, plus `org-c.fgentic.localhost` as a denied control. The delegation proof verifies the ES256/JCS card at `GET https://a2a.org-a.fgentic.localhost/api/a2a/kagent/docs-qa/.well-known/agent-card.json`, obtains a short-lived org-B JWT, and calls only the matching `POST` route. Wrong credentials, audience, client, method, path, or budget fail; one 3,000-token reservation succeeds through docs-qa and the deterministic model, and the second exceeds the 5,000-token `azp` window. The separate aggregate model metric must increase, but it is not presented as per-consumer usage.

The Matrix proof requires room-v12 policy, participant-only server ACLs, bidirectional messages between A and B, rejected join plus signed-federation-send attempts from C, and a Synapse callback dropping a disallowed event before it reaches A. `mise run fed:policy-reload` additionally proves a git policy change takes effect through Flux without restarting either Synapse pod and restores the canonical deny policy. The cluster stays running for inspection; remove only that lab with `mise run fed:down`. See the [federation lab topology and trust boundary](docs/federation.md#85-disposable-federation-hardening-lab); use the separate [partner onboarding runbook](docs/federation-onboarding.md) before enabling a real organization.

## Production

Production is a separate GitOps path: SOPS-encrypted secrets, a reviewed git source, the full observability and SSO layers, and Flux reconciliation. Follow the self-contained [production installation](docs/production.md), then the [security](docs/security.md), [identity](docs/identity.md), and [operator](.agents/skills/matrix-agents/SKILL.md) runbooks. Enable an external network only through the [opt-in interop contract](docs/interop.md); the [Slack provider walkthrough](docs/interop-slack.md) is separate because it requires a workspace owner and live evidence.

## Repository layout

```text
apps/matrix-a2a-bridge/  # the Go bridge (mautrix/go appservice + a2a-go client) + its deploy/ Flux unit
apps/synapse-federation-policy/ # standalone Python Synapse callback policy + namespace-neutral ConfigMaps
infra/{terraform,flux,gateway,postgres,matrix,keycloak,agentgateway,models,kagent,bridges,federation,secrets}
clusters/               # Flux entrypoints: base/ DAG + demo/, federation/, local/ (k3d), and gcp/ (GKE) overlays
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
