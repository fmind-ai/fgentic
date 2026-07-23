# Fgentic — Sovereign Agentic Collaboration Platform

[![CI](https://github.com/fmind-ai/fgentic/actions/workflows/ci.yml/badge.svg)](https://github.com/fmind-ai/fgentic/actions/workflows/ci.yml) [![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE) [![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](apps/matrix-a2a-bridge/go.mod) [![Status: experimental](https://img.shields.io/badge/status-experimental%20pre--1.0-orange.svg)](#project-status--roadmap)

> **Status: early / experimental (pre-1.0).** Live end-to-end on the local reference cluster, but APIs, manifests, and docs still move between milestones — see [project status & roadmap](#project-status--roadmap) before depending on it.

**Humans and AI agents in the same chat rooms. `@mention` an agent or use `/ask` to delegate a task. It replies. Self-hosted, out of the box, on open standards — you choose where your prompts go, and organizations can ultimately federate so agents collaborate across company boundaries.**

The agentic AI landscape is consolidating around closed, tenant-anchored platforms — Slack, Teams, and Google Chat all ship @mentionable agents, every one anchored to a vendor's tenant. **Fgentic** is the open counter-proposal: a **sovereign** platform an enterprise can self-host end-to-end — built exclusively from open protocols and genuinely open-source components, with every layer swappable, a per-cluster choice of model backend (self-hosted included), no proprietary SaaS in the critical path, and **federation** as the destination. It stitches together five open pieces, with their independent stewardship mapped in [Open Agentic Stack](docs/open-stack.md):

- **[Matrix](https://matrix.org)** — the collaboration fabric and UI (self-hosted **Synapse** + **Matrix Authentication Service**, with **Element** Web/X as clients). Matrix federation is the only cross-organization messaging federation proven in production at scale (Germany's healthcare system, NATO, Sweden's public sector).
- **[A2A](https://a2a-protocol.org)** (Agent2Agent, a Linux Foundation protocol, latest stable spec v1.0.1; AgentCard/service version `1.0`) — how tasks are delegated to agents, within and across organizations.
- **[kagent](https://kagent.dev)** (CNCF) — AI agents running natively on Kubernetes, each exposed as an A2A server.
- **[agentgateway](https://agentgateway.dev)** (Agentic AI Foundation) — the AI-native data plane: one governed chokepoint for locally hosted LLM/MCP/A2A traffic, where the platform's model credential lives.
- **A small Go bridge** — [`matrix-a2a-bridge`](apps/matrix-a2a-bridge/) — the novel glue that turns an `@mention` into either a local kagent call or an explicitly pinned remote A2A call, then posts the reply back. (As far as we can find, the first Matrix↔A2A appservice bridge.)

Agent-runtime swappability is continuously tested rather than assumed: the bridge integration gate completes the real Matrix appservice round trip against a standalone plain `a2a-go` server while installing no kagent resources.

Fgentic ships opt-in, digest-pinned GitOps units for **Slack** and **Telegram** on top of Matrix's upstream bridge ecosystem. They are disabled by default and require provider-owner, policy, and live bidirectional acceptance; rendered manifests are not a compatibility claim. Signal and WhatsApp remain evaluated candidates. Organizations already using Mattermost, Rocket.Chat, Zulip, or Microsoft Teams can [adopt Fgentic beside, bridge selectively, or migrate](docs/chat-coexistence.md); Teams is explicitly [coexistence under review](docs/adr/0011-teams-coexistence-not-bridge.md), not a promised Teams↔Matrix bridge.

> **New here?** The specification lives under [docs/](docs/), split by topic: [architecture & vision](docs/architecture.md), [open-stack governance](docs/open-stack.md), [design decisions D1–D20](docs/design-decisions.md), [identity and SSO](docs/identity.md), [IdP group sync](docs/group-sync.md), [model provider profiles](docs/models.md), [bridge behavior](docs/bridge.md), [external-network interop](docs/interop.md), [security model](docs/security.md), [federation design](docs/federation.md), [partner federation onboarding](docs/federation-onboarding.md), [observability](docs/observability.md), [licensing strategy](docs/licensing.md), [exit strategy](docs/exit-strategy.md), [incumbent-chat coexistence](docs/chat-coexistence.md), [roadmap history](docs/roadmap.md) — plus the [ADRs](docs/adr/). The executable roadmap is the set of [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones).

> **Browsing or authoring documentation?** Markdown under `docs/` remains canonical. Build every page with `mise run docs:build`, preview locally with `mise run docs:serve`, and see the [documentation-site delivery contract](docs/site.md) for the human-gated GitHub Pages publication step.

> **Evaluating adoption?** Start with the vendor-neutral [adopter decision brief and parameterized TCO worksheet](docs/adopter-decision-brief.md); it separates the controls Fgentic owns from the open-source systems it composes and requires your own measured usage, labor, and contracted rates.

> **Moving from a tenant-anchored agent platform?** Use the [inbound migration guide](docs/migration-guide.md) to rebuild identity and authorization, parallel-run one Agent, cut over per room or organization, preserve rollback, and decommission the old scope deliberately.

> **Joining an evaluation?** Choose the [security lead, DPO, platform engineer, developer, or end-user onboarding path](docs/onboarding/index.md) for a short route to the controls and evidence your role owns.

---

## Core principles

1. **Sovereignty first.** Self-hosted on any conformant Kubernetes; the model backend is a per-cluster choice ranked by sovereignty — self-hosted vLLM (prompts never leave the cluster) > EU API (Mistral) > hyperscalers (Vertex/Anthropic/OpenAI) — and every layer has a documented [exit](docs/exit-strategy.md).
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
                          │  A2A SendMessage ── through agentgateway ──▶ kagent agent
                          ◀── reply text ─────────────────────────────────┘
                          │  post as ghost @agent-k8s (reply to the original message)
        ▼
Human in Element:  "@agent-k8s: The container is OOMKilled — memory limit 128Mi …"
```

Data-flow details and async/long-task behavior: [docs/bridge.md](docs/bridge.md); the layer map: [docs/architecture.md](docs/architecture.md).

The plaintext command fallback works in Matrix clients without ghost-MXID autocomplete: `/agents` lists the agents you may invoke, `/ask <agent> <prompt>` uses the same policy and cost-admission path as an `@mention`, and `/budget` shows the current request limits plus remote per-request token reservation ceilings. The budget view is read-only and never presents a reservation as observed consumption. Native Matrix command-picker work remains tracked separately in [#223](https://github.com/fmind-ai/fgentic/issues/223).

## Architecture at a glance

| Layer                      | Component                                                                                                      | Namespace             |
| -------------------------- | -------------------------------------------------------------------------------------------------------------- | --------------------- |
| UI + collaboration fabric  | Element Web/X · Synapse · MAS (via [ESS Community](https://github.com/element-hq/ess-helm))                    | `matrix`              |
| The bridge (the glue)      | `matrix-a2a-bridge` (Go, `mautrix/go` appservice + `a2a-go`)                                                   | `bridge`              |
| AI data plane / governance | agentgateway (LLM + A2A routing, credential chokepoint)                                                        | `agentgateway-system` |
| Agents                     | kagent (Agent CRDs served as A2A on `:8083`)                                                                   | `kagent`              |
| Optional network interop   | Digest-pinned mautrix Slack/Telegram appservices; disabled until selected and accepted                         | `bridges`             |
| Optional reference IdP     | Keycloak 26.7.0 via the KeycloakX chart                                                                        | `keycloak`            |
| Optional administrator UI  | Ketesa v1.3.0, locked to the local homeserver and authorized by Synapse/MAS ([runbook](docs/admin-console.md)) | `admin`               |
| Optional self-hosted model | vLLM CPU + pinned Qwen2.5-0.5B demo model                                                                      | `models`              |
| Optional grounding ingest  | Bounded Git/Markdown acquisition + Docling + authenticated embeddings + scoped state/DML writers               | `knowledge`           |
| Shared state               | CloudNativePG: scoped service databases plus the composed pgvector knowledge store                             | `postgres`            |
| Web ingress + TLS          | Gateway API (Traefik) + cert-manager (Let's Encrypt)                                                           | `gateway`             |
| Observability              | kube-prometheus-stack: Prometheus · Grafana · Alertmanager ([docs/observability.md](docs/observability.md))    | `monitoring`          |
| Runtime image security     | Trivy Operator: continuous HIGH/CRITICAL feed-drift reports and alert                                          | `trivy-system`        |
| Delivery                   | Flux v2 pull-based GitOps                                                                                      | `flux-system`         |

Before enabling ingestion, provision the operator-owned `knowledge-source-bundle` PVC under the deployment's storage, encryption, backup, retention, and access policy. The sole Git/Markdown connector writes only verified artifacts from this cluster's reconciled Flux source; ingestion mounts the claim read-only. Flux never owns or prunes the corpus claim. Non-public corpus bytes never belong in this public GitOps repository, a plaintext ConfigMap, or SOPS—the checked-in source-bundle ConfigMaps remain synthetic public offline fixtures.

Reference deployment: `fgentic.fmind.ai` (Element at `chat.`, Synapse at `matrix.`, MAS at `auth.`, optional Keycloak IdP at `id.`, optional Ketesa at `admin.`; user IDs `@name:fgentic.fmind.ai` via apex `.well-known` delegation).

## Key decisions (the short version — details in [docs/design-decisions.md](docs/design-decisions.md))

1. **Homeserver: Synapse + MAS + Element via ESS Community, deliberately.** We evaluated the whole 2026 homeserver landscape (Tuwunel, continuwuity, Conduit, chart-less Synapse, Palpo) and stayed: ESS wins on everything this platform lives on — appservice API maturity, modern auth (MAS) + Element X, federation policy hooks, and PostgreSQL. ESS Community is AGPL and open-core (Element gates HA, LDAP, and its federation border gateway behind ESS Pro) — we say that openly, keep a documented **Apache-2.0 fallback profile** (Tuwunel/continuwuity) for AGPL-averse deployments, and define concrete triggers for ever switching ([docs/licensing.md](docs/licensing.md)).
1. **License: Apache-2.0, DCO, no CLA.** Patent grant, foundation-donation fit (CNCF/AAIF), coherence with A2A/MCP/kagent/agentgateway. The bridge embeds `mautrix/go` (MPL-2.0) — attribution ships in [NOTICE](NOTICE).
1. **Federation honesty.** Matrix federation gives _organization-level_ cryptographic identity (a partner's server vouches for its users), full room replication to every participating server, and best-effort cross-server redaction. We design for that reality — closed federation, room v12, server ACLs, per-agent sender allowlists, policy-as-code borders, and A2A v1.0 Signed AgentCards for per-agent identity — instead of overclaiming ([docs/federation.md](docs/federation.md)).
1. **Cost is a first-class failure mode.** Every mention is an LLM invocation, so the bridge rate-limits per sender and per room, and agentgateway meters tokens and spend — the closest prior-art project died of exactly this.

## Project status & roadmap

**Live end-to-end on the local reference cluster.** A Matrix `@mention` in Element produces a real LLM-backed agent reply — through the bridge, agentgateway (no agent ever holds a model key), and kagent, with conversation threading, rate limits, sanitized failure replies, and Prometheus/Grafana observability (bridge delegation metrics + gateway GenAI token metering + the LLM spend alert). The local/GCP GitOps profiles additionally carry a structurally optional, digest-pinned Trivy layer and runtime image-vulnerability drift alert. The provider-free bridge fixture also proves an explicitly pinned remote URL round-trips only with a valid A2A v1.0 Signed AgentCard and then fails closed after post-signature tampering; no production remote is enabled by default. The opt-in federation lab proves the inbound complement: org B obtains a client-credentials JWT, invokes only org A's signed docs-qa route under an `azp`-scoped token reservation, reaches the deterministic model while direct kagent remains unpublished, and verifies a task-bound seller-signed receipt without presenting the reservation as consumed usage. Every layer reconciles from this repository via Flux; the same manifests drive the GKE reference profile (`clusters/gcp`). The adversarial-review fixes (D1–D15) are all implemented and unit-tested ([docs/design-decisions.md](docs/design-decisions.md)).

**The roadmap lives on the current [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones)** (each with an epic tracker issue; that page is the source of truth), sequenced sovereignty-first. The dated history through M24 is summarized in [docs/roadmap.md](docs/roadmap.md); later milestones remain discoverable from GitHub without requiring this pointer to name a fixed ceiling. Pickup is scoped by the `track/*` labels rather than raw milestone order: `track/v1` (the [Definitive v1 focus board #316](https://github.com/fmind-ai/fgentic/issues/316)) comes first, ordered `priority/p0 → p1 → p2`, and `track/vision` waits until v1 ships. Issues labeled `agent-ready` are groomed for autonomous coding agents; `needs-human` marks decisions, approvals, or spend.

## Evaluate in 15 minutes

Prerequisites: Docker (allocate at least ~8 GiB RAM and 4 CPUs to it), Git, [mise](https://mise.jdx.dev/), and at least 10 GiB of free disk for the pinned cluster images. The single-organization demo is the smallest laptop profile: it omits SSO, telemetry, tracing, and vulnerability scanning, scales the kagent UI to zero, and disables KMCP while retaining the complete Matrix → bridge → agentgateway → kagent path. Use the production-shaped local profile only when testing those omitted controls. The demo binds host ports **80** and **443** on `127.0.0.1` and serves the platform under `*.fgentic.localhost`, so both ports must be free and `*.localhost` must resolve to loopback (automatic on systemd-resolved and macOS; otherwise add a hosts entry). The default is deliberately free and deterministic: it uses an in-cluster OpenAI-compatible response stub and proves integration, not model quality.

```bash
mise install
mise run demo:up
```

The final output is the Element URL, `@alice:fgentic.localhost`, its generated password, and the seeded `#lobby:fgentic.localhost` room. Every mapped ghost is already a member and has replied to its own seeded transport probe. The command does not mutate the checkout, commit, push, or need a GitHub account; its random credentials live only in the `fgentic-demo` cluster. Set `FGENTIC_DEMO_CACHE_DIR` to a persistent directory to reuse BuildKit layers across repeated installs. Remove only that evaluation cluster with `mise run demo:down`. If teardown is interrupted, rerun the same command: it resumes only the exact identities recorded before deletion, while `demo:status` reports the pending recovery and `demo:up` refuses reuse. The content-free receipt lives under `XDG_STATE_HOME/fgentic/cluster-teardown/` (or `~/.local/state/fgentic/cluster-teardown/` when XDG state is unset); `FGENTIC_DEMO_STATE_DIR` overrides the state root. For the persistent full `fgentic` cluster, `mise exec -- k3d cluster stop fgentic`/`mise exec -- k3d cluster start fgentic` preserve its state; `mise run cluster:down` deletes it.

Choose the model boundary before using non-demo data:

| Choice                       | Sovereignty and cost boundary                                                                                                                     |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| `demo` (evaluation default)  | Deterministic cluster-only stub; no model credential, prompt egress, or token charge; not a real language model                                   |
| `vertex` (local/GCP default) | Vertex AI `google/gemini-2.5-flash`; the credential stays at agentgateway, while prompts leave the cluster and the selected GCP project is billed |
| `vllm`                       | Real self-hosted model; strongest sovereignty, but roughly 2.7 GB of downloads and 4–6 GiB RAM                                                    |
| `mistral`                    | EU-hosted API path; prompts leave the cluster and the selected account is billed                                                                  |
| `anthropic`/`openai`         | Hyperscaler API path; residency and billing depend on the selected account/profile                                                                |
| `azure-openai`               | Azure deployment boundary; region/data-zone selection and billing remain account controls                                                         |

For example, `FGENTIC_LLM_PROVIDER=vllm mise run demo:up` selects the real credential-free self-hosted profile. Vertex defaults to `google/gemini-2.5-flash` and uses ADC locally; Terraform grants the exact GKE agentgateway proxy Workload Identity direct Vertex access without a Google service account or key. Live GKE proof remains spend-gated in [#59](https://github.com/fmind-ai/fgentic/issues/59). Mistral, Anthropic, OpenAI, and Azure OpenAI require the matching key and an explicit `FGENTIC_LLM_MODEL`. Every paid provider requires `FGENTIC_ALLOW_PAID_PROVIDER=yes` in the disposable demo lifecycle. See the complete [provider contract](docs/models.md). Do not use evaluation credentials or the deterministic stub in production.

To exercise the federation thesis without an external model or provider account, use the canonical profile:

```bash
mise run fed:up
```

It creates a separate `fgentic-fed` cluster with participating Synapse homeservers at `org-a.fgentic.localhost` and `org-b.fgentic.localhost`, plus `org-c.fgentic.localhost` as a denied control. The delegation proof verifies the ES256/JCS card at `GET https://a2a.org-a.fgentic.localhost/api/a2a/kagent/docs-qa/.well-known/agent-card.json`, obtains a short-lived org-B JWT, and calls only the matching `POST` route. Wrong credentials, audience, client, method, path, or budget fail; one 3,000-token reservation succeeds through docs-qa and the deterministic model, and the second exceeds the 5,000-token `azp` window. The successful terminal A2A Task metadata carries one seller-signed receipt bound to that canonical request and task identity. Org B verifies the independent JWK, rejects a byte-tampered receipt, and proves denied requests do not append to the content-free single-writer archive. `tokensConsumed` remains null until per-consumer actuals exist. The separate aggregate model metric must increase, but neither it nor the reservation is presented as per-consumer consumption.

On a memory-constrained laptop, explicitly opt into `mise run fed:up:constrained`. It keeps the same three Synapse servers, shared Postgres, Keycloak, Traefik, Flux, agentgateway, docs-qa/kagent runtime, and acceptance proofs while applying lab-sized workload/controller tuning, a 1 GiB soft target for the disposable k3s server, pausing the optional metrics-server, and serializing the expensive first installs. `kubectl top` is therefore unavailable while the constrained profile is running. The k3s target is creation-time state, so switch between canonical and constrained capacity with `mise run fed:down`; same-mode `fed:stop`/`fed:up` still reuses the exact cluster and image volume. The constrained wait fails after 20 minutes without a new immutable Flux convergence milestone or after 60 minutes total, whichever happens first. Set `FGENTIC_FED_TRACE=yes` on either `fed:up` command to write an allowlisted, content-free resource trace under `.agents/tmp/federation-resources/`; the exact tuning, measurement method, and laptop budget are defined in [§8.5.2](docs/federation.md#852-constrained-host-profile-evidence-and-lifecycle).

The Matrix proof requires room-v12 policy, participant-only server ACLs, bidirectional messages between A and B, rejected join plus signed-federation-send attempts from C, and a Synapse callback dropping a disallowed event before it reaches A. `mise run fed:policy-reload` additionally proves a git policy change takes effect through Flux without restarting either Synapse pod and restores the canonical deny policy. The cluster stays running for inspection: `mise run fed:status` reports its capacity mode, state, and retained bytes, while `mise run fed:stop` releases CPU/RAM and preserves the exact owned cluster and image volume for the next same-mode `fed:up`. `mise run fed:down` removes all lab-owned containers, network, image volume, and locally built images. An interrupted teardown is retryable through its exact-identity receipt; pending `fed:status` is inspect-only and `fed:up` fails closed until `fed:down` completes. The constrained path creates no additional persistent cache automatically; an explicitly configured `FGENTIC_DEMO_CACHE_DIR` remains caller-owned. See the [federation lab topology and trust boundary](docs/federation.md#85-disposable-federation-hardening-lab); use the separate [partner onboarding runbook](docs/federation-onboarding.md) before enabling a real organization.

## Develop with the smallest sufficient loop

The repository owns its development cluster; no global Kubernetes setup, default kubeconfig context, GitHub account, SOPS key, or paid model is required. Use the cheapest proof that reaches the boundary you changed:

1. **Authoring an agent:** `mise run agent:new <name>` scaffolds it, then `mise run agent:test <name>` runs that one agent's golden tasks against the deterministic zero-spend model — offline, no cluster — the same code path as CI's `test:agents-golden`. The full scaffold → edit → test → promote → roll-back loop is the [agent authoring runbook](.agents/skills/matrix-agents/SKILL.md#runbook-add-an-agent).
1. **Go behavior:** run the focused package tests, then the bridge suite before commit.

   ```bash
   mise --cd apps/matrix-a2a-bridge exec -- go test ./internal/a2aclient/ ./internal/bridge/
   mise run test:app
   ```

1. **Matrix ↔ A2A wire behavior:** `mise run test:integration` creates and removes its own isolated kind fixture; it does not need the platform cluster.
1. **Interactive bridge work:** create the lightweight cluster once with `mise run dev:up`, then use `mise run dev:reload` after a code change or `mise run watch` for automatic reloads. Reuse is bridge-only: it does not rebuild the local Git source, reinstall Flux, reconcile the platform, or reseed the room.
1. **Manifest/profile or final end-to-end proof:** run `mise run demo:up`; this intentionally reconciles the current checkout and repeats admission plus seeded Matrix → bridge → agentgateway → kagent acceptance.
1. **Full platform-only features:** use `mise run cluster:up` and the production-shaped bootstrap only for Keycloak SSO, observability/tracing, Trivy, SOPS, or full Flux behavior omitted from the demo.

`mise run dev:status` reports the lightweight cluster, `mise run dev:stop` releases its active CPU/RAM while preserving state and images, and `mise run dev:down` deletes only that owned cluster. All `dev:*` commands use a temporary kubeconfig and reject a same-named cluster without the demo ownership label. Docker Desktop on macOS and Docker Engine on Linux are both supported through the repo-pinned mise toolchain; ports 80/443 on `127.0.0.1` must be free while the demo is running.

## Develop with coding agents

Fresh clones and worktrees use one non-mutating bootstrap:

```bash
mise run agent:setup
```

It installs the pinned root and application toolchains, downloads both Go modules, and syncs the locked Python environment. It does not install Git hooks, rewrite dependency manifests, create a cluster, load credentials, or select a paid provider.

Both local agent products can create worktrees automatically:

1. **Claude Code CLI:** run `claude --worktree <name>`. Claude creates `.claude/worktrees/<name>` from fresh `origin/HEAD`; the tracked `SessionStart` hook runs `agent:setup` automatically. Run plain `claude` once first to accept workspace trust. See [Claude Code worktrees](https://code.claude.com/docs/en/worktrees).
1. **Codex App:** select **Worktree** for the task and choose the checked-in Fgentic local environment. Codex creates its managed worktree and `.codex/environments/environment.toml` runs `agent:setup`. This worktree mode is in the Codex App, not Codex CLI. See [Codex worktrees](https://developers.openai.com/codex/environments/git-worktrees) and [local environments](https://developers.openai.com/codex/environments/local-environment).
1. **Codex CLI:** use an existing worktree/clone with `codex -C <path>`. The CLI does not create a worktree automatically. A second full clone is safe and simple when duplicated Git objects and dependencies are acceptable; otherwise use `git worktree add` once or use the Codex App.

For Codex Cloud or Claude Code on the web, configure this repository setup script in the provider environment:

```bash
set -eu
curl https://mise.run | sh
export PATH="$HOME/.local/bin:$PATH"
printf '\nexport PATH="$HOME/.local/bin:$PATH"\n' >> "$HOME/.bashrc"
mise trust --all --yes
mise run agent:setup
```

Keep provider credentials, ADC, SOPS keys, kubeconfigs, and local platform overrides out of hosted environments. The repository contains the shared `AGENTS.md`, `CLAUDE.md`, project skills, attribution-safe Claude settings, and deterministic checks needed by fresh hosted clones. See [Codex Cloud environments](https://developers.openai.com/codex/environments/cloud-environment) and [Claude Code on the web](https://code.claude.com/docs/en/claude-code-on-the-web).

Use cloud agents by default for parallel code, documentation, unit-test, and PR work that should continue without your laptop. Use local agents only when the task needs Docker/k3d, ignored local configuration, or machine-specific acceptance. Multiple worktrees or clones may read the same cluster, but **exactly one path owns mutations at a time**: image builds/imports and `dev:*`, `demo:*`, `fed:*`, `cluster:*`, kind, or runtime-test lifecycles are serialized because their Docker daemon, cluster names, image tags, and host ports are shared.

## Production

Production is a separate GitOps path: SOPS-encrypted secrets, a reviewed git source, the full observability and SSO layers, and Flux reconciliation. Follow the self-contained [production installation](docs/production.md), then the [security](docs/security.md), [identity](docs/identity.md), and [operator](.agents/skills/matrix-agents/SKILL.md) runbooks. Enable an external network only through the [opt-in interop contract](docs/interop.md); the [Slack provider walkthrough](docs/interop-slack.md) is separate because it requires a workspace owner and live evidence.

Running Fgentic under your own GitHub org, domain, GCP project, and registry? The [forking & adapting checklist](docs/forking.md) enumerates every identifier to change (most are one line in `platform-settings`).

## Repository layout

```text
apps/matrix-a2a-bridge/  # the Go bridge (mautrix/go appservice + a2a-go client) + its deploy/ Flux unit
apps/synapse-federation-policy/ # standalone Python Synapse callback policy + namespace-neutral ConfigMaps
apps/activitypub-agent-gateway/ # experimental 2nd federation transport (ActivityPub); demo-reconciled with public routing disabled (ADR 0014)
apps/matrix-group-sync/   # opt-in Keycloak-group to managed Matrix-room reconciler; audit-only by default
evals/                  # per-Agent deterministic golden.json fixtures; run with mise run test:agents-golden
infra/{namespaces,terraform,flux,gateway,postgres,matrix,admin,keycloak,agentgateway,mcp-catalog,models,kagent,knowledge,bridges,federation,policies,production-ha,observability,trivy-operator,secrets}
clusters/               # Flux entrypoints: base/ DAG + demo/, federation/, federation-constrained/, local/ (k3d), and gcp/ (GKE) overlays
docs/                    # the specification split by topic (architecture, decisions, security, federation, …) + docs/adr/
.github/                 # CI (mise gates) + CD (signed, digest-pinned bridge image) + issue/PR templates
.agents/                 # canonical agent instructions, shared skills, and operator runbooks
.claude/                 # Claude skills bridge, fresh-worktree setup, and attribution policy
.codex/                  # Codex App local environment (automatic worktree setup)
AGENTS.md                # root discovery link to .agents/AGENTS.md
CONTRIBUTING.md          # how to contribute (workflow, labels, DCO) — with GOVERNANCE, SECURITY, MAINTAINERS, ADOPTERS
```

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for the workflow (issues, labels, DCO sign-off) and [GOVERNANCE.md](GOVERNANCE.md) for how the project is run; all participants are expected to follow our [Code of Conduct](CODE_OF_CONDUCT.md). The short version:

1. Start with [docs/](docs/) — the [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones) are the backlog; [design decisions](docs/design-decisions.md) and ADRs in [docs/adr/](docs/adr/) capture what is settled (propose a new ADR to revisit one).
1. The warning-free `check` and `test` gates must pass; installed git hooks serialize them across worktrees, while hookless environments run `mise run agent:gate` once near PR readiness.
1. Conventions live in [.agents/AGENTS.md](.agents/AGENTS.md) (they bind human and AI contributors alike): Go, type-safe, small composable units, no tech debt, Conventional Commits, DCO sign-off.
1. Never commit plaintext secrets — SOPS-encrypted `*.sops.yaml` only (gitleaks runs pre-commit).

Security reports go through [SECURITY.md](SECURITY.md), not public issues. Deployments and pilots: add yourself to [ADOPTERS.md](ADOPTERS.md).

Questions, usage support, and early ideas follow the [project support routes](.github/SUPPORT.md).

## Standards & building blocks

Matrix (spec.matrix.org) · A2A (a2a-protocol.org, Linux Foundation) · MCP (Agentic AI Foundation) · Kubernetes · Gateway API · OpenID Connect (via MAS) · SOPS/age. Everything is open source and self-hostable.

## License

[Apache-2.0](LICENSE) © Médéric Hurier (Fmind) — chosen for its explicit patent grant, foundation-donation requirements (CNCF/AAIF), and coherence with the A2A/MCP/kagent/agentgateway stack ([docs/licensing.md](docs/licensing.md)). The bridge embeds `mautrix/go` (MPL-2.0) — the third-party [NOTICE](NOTICE) ships with the binary image. Contributions use DCO sign-off.
