# AGENTS.md (Project) — Fgentic

An open-standard, **federated** AI-agent collaboration platform: humans and AI agents share **Matrix** rooms, `@mention` to delegate tasks over **A2A** to **kagent** agents, governed by **agentgateway**, stitched by a small **Go bridge** — with Matrix federation as the path to cross-organization agent collaboration (the project's thesis: the open alternative to closed, tenant-anchored agent platforms). Kubernetes-native, Flux GitOps. Read [SPEC.md](../SPEC.md) first — the binding specification and plan (design decisions D1–D15 with evidence, security model §7, federation design §8, licensing + homeserver strategy §10, roadmap §13); [PLAN.md](../PLAN.md) is the original research record. Where they disagree, SPEC.md wins. Status: **Phase 0 done** (all review findings fixed in code, unit-tested, CI/CD in place), **not yet deployed** — Phase 1 (first deployment of the Matrix layer) is next.

## Layout

- `apps/matrix-a2a-bridge/` — the custom Go bridge (a Matrix Application Service using `mautrix/go` + the `a2a-go` client). Self-contained: own Go module, Dockerfile, Helm chart, and `deploy/` (its Flux unit — Namespace + HelmRelease, reconciled by the `bridge` Kustomization).
- `clusters/` — Flux entrypoints: `base/` (the shared Kustomization DAG reconciling `infra/` + `apps/`, parameterized by flux post-build substitution) + per-cluster overlays `local/` (k3d) and `gcp/` (GKE reference), each carrying its `platform-settings` ConfigMap (domain, GCP project, TLS issuer, model).
- `.github/workflows/` — CI (`ci.yml`: the same `mise run` gates as the git hooks + clean-tree assert) and CD (`cd.yml`: bridge image build → trivy scan → cosign sign → digest committed back into `deploy/helmrelease.yaml`).
- `infra/terraform/` — GKE reference cluster (private nodes + Cloud NAT, Workload Identity, CNPG backups bucket; `bootstrap/` = one-time tfstate bucket, apply it first). Workloads stay provider-independent.
- `infra/flux/` — platform Helm layer (HelmRepositories/OCIRepositories + HelmReleases).
- `infra/gateway/` — Gateway API resources + Let's Encrypt TLS (`chat.` / `matrix.` / `auth.fgentic.fmind.ai`).
- `infra/postgres/` — shared CloudNativePG cluster + databases/roles (`synapse`, `mas`, `bridge`, `kagent`).
- `infra/matrix/` — ESS Community `matrix-stack` (Synapse + MAS + Element Web + well-known delegation), pinned + values validated against the chart schema (it changes between CalVer releases).
- `infra/agentgateway/` — AI-native data plane: LLM egress chokepoint + A2A route to kagent.
- `infra/kagent/` — kagent platform + sample Agents (`a2aConfig`, served as A2A on `:8083`).
- `infra/bridges/` — (phase 4) off-the-shelf mautrix bridges (Slack/Telegram/…) for external-network interop.
- `infra/secrets/` — SOPS-encrypted secrets (`*.sops.yaml`) + `*.example` templates.
- `docs/adr/` — Architecture Decision Records.
- `.agents/skills/matrix-agents/` — operator runbooks (bootstrap, add-agent, add-bridge, DNS/TLS).
- `mise.toml` — root task vocabulary + pinned toolchain. `SPEC.md` (the spec + plan) / `PLAN.md` (original research) / `README.md` (humans).

## Platform protocols (how agents participate at runtime)

1. **Identity.** Every platform agent is a Matrix ghost `@agent-<name>:<server>` owned by the bridge (exclusive appservice namespace `@agent-.*`). The bridge maps ghosts to kagent agents via an explicit allowlist (`agents.yaml`) — unmapped targets are rejected.
1. **Delegation.** An `@mention` (typed `m.mentions`, plaintext fallback) becomes an A2A `message/send` to `http://kagent-controller.kagent:8083/api/a2a/<namespace>/<name>` (AgentCard at `…/.well-known/agent-card.json`), routed through agentgateway (`http://agentgateway-proxy.agentgateway-system:8080`). Non-streaming by design; long tasks use `tasks/get` polling + Matrix `m.replace` edits (SPEC §6).
1. **Threading.** The bridge threads conversations with a per-`(room, ghost)` A2A `contextId`; kagent maps `contextId` 1:1 to a persistent session. Do not reuse a contextId across different agents.
1. **Attribution.** The Matrix sender is forwarded as the A2A user identity (`X-User-Id`); every invocation is auditable.
1. **Trust boundaries.** Room content is untrusted input to agents (prompt injection is the #1 threat — SPEC §7): sender allowlists gate who may invoke which agent; agents from other homeservers are never resolved as local targets; agent replies are `m.notice` and other automation must not act on them. LLM egress goes only through agentgateway — no agent holds a model credential.

## Principles

1. **Open standards only.** Every layer is an open protocol or OSS component (Matrix, A2A, MCP, Kubernetes, Gateway API, OIDC). No proprietary SaaS in the critical path; every component is swappable. This is the whole point of the showcase.
1. **Federation-ready decisions.** The endgame is cross-organization collaboration (SPEC §8): federated rooms use room v12+, closed-federation allowlists, and server ACLs; nothing merged may assume "single homeserver forever" (e.g., never match users by localpart without checking the homeserver).
1. **Independent apps, bridging infra.** Each app under `apps/` is self-contained and deployable on its own. `infra/` exposes shared components (Postgres, gateway, agentgateway) without coupling apps together. The homeserver, MAS, bridge, and kagent each get their **own database + scoped role** in the shared CloudNativePG cluster (Synapse's DB uses `C` collation).
1. **Flux GitOps delivery.** Production CD is Flux v2, pull-based, for both `infra/` and `apps/`. Never `kubectl apply` / `helm upgrade` prod by hand — commit to git and let Flux reconcile. GitHub Actions is CI-only (build/test/scan/sign the bridge image, commit the digest). `HelmRelease.dependsOn` references HelmReleases only — wrap apps in Flux Kustomizations to depend on Kustomizations.
1. **Helm-first reusable manifests.** Package Kubernetes manifests as parameterised Helm (the bridge's `chart/`) or Flux `HelmRelease`s with inline `spec.values`; per-directory `kustomization.yaml` lists resources; `base` + overlays where environments differ. Kustomize is Flux's thin resource-lister, never a Helm replacement.
1. **Gateway API, not Ingress.** Web/UI traffic (Element, Synapse client API, MAS) routes through the Gateway API (Traefik); cert-manager terminates Let's Encrypt TLS per host. **Agent** traffic (A2A/LLM/MCP) egresses through **agentgateway on `ClusterIP`** (not internet-exposed, wrapped in a `NetworkPolicy`) — the single model-credential chokepoint. kagent's A2A endpoint is unauthenticated by default, so the NetworkPolicies on **both** the agentgateway and kagent namespaces are load-bearing security controls.
1. **A2A for delegation, non-streaming.** Agents are invoked over A2A `message/send` (request/response; `tasks/get` polling for long tasks). Streaming is deliberately unused. Reuse the official `a2a-go` SDK (the same one kagent uses) — do not hand-roll JSON-RPC.
1. **Homeserver strategy is decided — don't relitigate it per-PR.** Reference profile: **ESS Community** (Synapse + MAS + Element; best appservice API, MAS/Element X, Postgres). Pure-permissive fallback profile: **Tuwunel/continuwuity** (Apache-2.0, RocksDB) for AGPL-averse deployments. The bridge only uses stable-spec appservice endpoints, so it stays homeserver-portable; switching is governed by the explicit triggers in SPEC §10.3, not preference.
1. **Agent rooms unencrypted, by policy.** Collaboration/agent rooms are unencrypted, force-disabled server-side via `/.well-known/matrix/client`; the bridge does not wire the crypto package. Documented, revisitable ([ADR 0008](../docs/adr/0008-unencrypted-agent-rooms.md)) — and mandatory to revisit for federated rooms (SPEC §8.4).
1. **Cost is a failure mode.** Every mention is an LLM invocation: keep the bridge's per-sender/per-room rate limits and agentgateway's token metering intact; never merge a change that lets automation invoke agents unboundedly (SPEC D7/D8).
1. **SOPS + age secrets.** Never commit plaintext secrets — only `*.sops.yaml`, decrypted in-cluster by Flux. A gitleaks pre-commit hook enforces it.
1. **GHCR images.** The bridge image is multi-arch distroless, published to `ghcr.io/fmind/matrix-a2a-bridge`, deployed by immutable digest (never `latest`).
1. **License hygiene.** Project code is **Apache-2.0** (SPEC §10). Never add an AGPL dependency to the bridge (mautrix/go is MPL-2.0 — keep the `NOTICE` files current); never mirror AGPL images (Synapse, Grafana) into project registries — reference upstream. Contributions use DCO sign-off, no CLA.

## Conventions

- Server name / domain (reference deployment): `fgentic.fmind.ai`. Bot user `@a2a-bridge:fgentic.fmind.ai`; agent ghosts `@agent-<name>:fgentic.fmind.ai` (appservice exclusive namespace `@agent-.*`).
- kagent A2A endpoint: `http://kagent-controller.kagent:8083/api/a2a/<namespace>/<name>` (AgentCard at `…/.well-known/agent-card.json`). agentgateway LLM: `http://agentgateway-proxy.agentgateway-system:8080`.
- Go (default) and Python are the core languages; Go for the bridge. Type-safe, small composable units, errors wrapped with `%w`, no ignored `err`, no tech debt.
- `mise` is the single source of truth for tasks (`install`, `format`, `check`, `test`, `build`, `watch`); lefthook + CI reuse it. Conventional Commits; no attribution.
