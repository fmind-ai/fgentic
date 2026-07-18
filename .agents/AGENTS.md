# AGENTS.md (Project) — Fgentic

Fgentic is an open-standard, sovereignty-first AI-agent collaboration platform: humans and AI agents share Matrix rooms, and an `@mention` delegates over A2A to a governed local kagent or an explicitly pinned remote agent. The platform is Kubernetes-native and Flux-delivered. Read the topic specs in [docs/](../docs/) before changing settled behavior; use [docs/agent-reference.md](../docs/agent-reference.md) for the detailed status, SPEC § mapping, repository topology, protocol constants, and rationale moved out of this concise entry point.

These instructions are binding repository-wide. A nested `AGENTS.md` adds app-specific guidance and wins only for its subtree.

## Start here: commands and verification

- Read the skill matching the task before editing: `github-flow` (issues, PRs, CI/CD), `docs-spec` (topic specs and ADRs), `bridge-dev` (Go bridge), `flux-gitops` (manifests and pins), `local-cluster` (k3d), `matrix-agents` (runbooks), `sops-secrets`, `terraform-gke`, or `observability`. Skills live under `.agents/skills/`.
- Fresh clones and worktrees use `mise run agent:setup`. It installs pinned tools and locked dependencies without hooks, manifest rewrites, cluster creation, credentials, or provider access. Do not substitute mutation-capable `mise run install` in a disposable setup.
- Run the smallest focused check during development. Most tasks are in the root `mise.toml`; app-only tasks run as `mise --cd apps/<app> run <task>`.
- Installed commit and push hooks serialize warning-free `mise run check` and `mise run test` through a host-local mutex. Let the hooks run; do not launch the aggregate gates first. In a hookless environment, run `mise run agent:gate` once near PR readiness. Re-run only the affected gate after an invalidating change.
- Worktrees isolate source, not Docker, k3d, kind, image tags, ports, or cluster names. At most one designated runtime owner may build/import shared images or run `dev:*`, `demo:*`, `fed:*`, `cluster:*`, or runtime Kubernetes tests. Everyone else stays on focused offline checks.
- Use the smallest sufficient runtime boundary when you own the lease: focused tests first; `test:integration` for the isolated Matrix↔A2A boundary; `dev:up` plus `dev:reload`/`watch` for bridge iteration; `demo:up` for profile acceptance; `fed:up` for federation; `cluster:up` only for controls omitted from demo. The canonical and constrained federation profiles must prove the same behavior.
- After recreating the local cluster, run `mise run cluster:overrides` to restore the gitignored `platform-settings-overrides` ConfigMap. Repository development must never rely on the user's default Kubernetes context.

## Roadmap and issue workflow

- Current GitHub milestones are the executable roadmap; [docs/roadmap.md](../docs/roadmap.md) is the dated map. Pickup is gated by track labels, not raw milestone order. Work `track/v1` first; do not pick up `track/vision` until v1 ships. Within a track, order `priority/p0` → `priority/p1` → `priority/p2` and sweep wedge epics top-to-bottom.
- `agent-ready` means the issue is groomed for autonomous completion, including peer review. `needs-human` is reserved for a genuine terminal gate: spend; an external account or identity action; publication or an upstream PR under the maintainer's name; an external counterparty; legal publication sign-off; an accepted strategic/policy ADR; or hard upstream readiness. A `Human:` bullet names that step. Acceptance checks, internal review, and local-cluster evidence are not `needs-human` gates.
- Issue claims are cooperative leases. Before editing, check assignees, `status/in-progress`, comments, branches, worktrees, and open PRs. Claim exactly one issue: assign yourself, add `status/in-progress`, and leave a UTC-stamped comment naming the session and branch. Re-read immediately and stop if a competing claim appeared. A claim is stale only after 12 hours without a heartbeat, branch, commit, or PR; document any takeover and release abandoned claims.
- Follow every issue's Tasks and Acceptance criteria literally. Branch `<type>/<slug>` from current `main` before the first commit; use Conventional Commits with DCO sign-off and no attribution. Keep one concern per branch.
- Open a PR with What / Why / How / Test plan, `Fixes #N`, and the appropriate lane label. Rebase on current `main` before pushing shared-file changes. Watch exact-head CI, obtain peer-agent review, address findings, and squash-merge only when the pushed head is green and no blocker remains.
- The M8 federation topology is a prerequisite for later M8 policy or cross-org claims. Never merge a single-homeserver assumption. Hosted agents work only an already-claimed issue and prepare-then-list any runtime proof they cannot execute; see [docs/hosted-agents.md](../docs/hosted-agents.md).

The detailed label taxonomy, milestone links, claim protocol, review gate, and `SPEC §N` mapping are preserved in [docs/agent-reference.md](../docs/agent-reference.md).

## Lean repository layout

- `apps/` — independent applications. `matrix-a2a-bridge` is the Go Matrix Application Service; `synapse-federation-policy` is the Python callback module; `activitypub-agent-gateway` is the separate additive ActivityPub transport. Each app owns its module, packaging, tests, and any nested `AGENTS.md`.
- `clusters/` — Flux entrypoints and environment overlays: `base`, `local`, `gcp`, `demo`, `federation`, and `federation-constrained`.
- `infra/` — reusable GitOps layers for namespaces, Matrix, Postgres, gateway, agentgateway, kagent, models, federation, policy, observability, identity, knowledge, optional bridges/admin, and production HA.
- `evals/` — per-Agent deterministic `golden.json` scenarios and rubrics. `scripts/new-agent.sh` scaffolds them; `mise run test:agents-golden` exercises every fixture against the zero-spend loopback model.
- `scripts/` — guarded setup, development, federation, validation, and acceptance commands. Lifecycle scripts own their named resources and must not read or switch the default Kubernetes context.
- `docs/` — OKF-frontmattered topic specs, runbooks, evidence, and ADRs. Preserve stable § numbering; revisit a settled decision with a new ADR.
- `.github/` — issue/PR templates and CI/CD workflows. CI reuses mise gates; CD builds, scans, signs, and digest-pins the bridge image.
- `.agents/` — canonical project instructions and shared skills. Root `AGENTS.md` is a symlink to this file; `.claude/skills` links to the same skill source.
- `mise.toml`, `lefthook.yml`, and formatter/security configs — the single task, hook, formatting, and scan vocabulary. `README.md` is the human entry point; community policy lives in `CONTRIBUTING.md`, `GOVERNANCE.md`, `MAINTAINERS.md`, `SECURITY.md`, and `ADOPTERS.md`.

The component-by-component layout and current milestone status are in [docs/agent-reference.md](../docs/agent-reference.md).

## Binding platform invariants

1. Open standards only. Keep the critical path on swappable OSS protocols and components: Matrix, A2A, MCP, Kubernetes, Gateway API, and OIDC.
1. Federation-ready by default. Federated rooms use room v12+, closed-federation allowlists, and server ACLs. Check a user's homeserver; never identify them by localpart alone.
1. Keep apps independent and shared infrastructure composable. Each homeserver, MAS, bridge, kagent, IdP, and enabled external bridge has its own database and scoped role. The knowledge store has separate owner and read-only retrieval roles.
1. Production delivery is pull-based Flux GitOps. Never `kubectl apply` or `helm upgrade` production by hand. Package reusable workloads Helm-first; use Kustomize as a resource/composition layer. `HelmRelease.dependsOn` targets HelmReleases, while app ordering belongs in Flux Kustomizations.
1. Use Gateway API, not Ingress. Local A2A, LLM, and MCP traffic stays behind agentgateway on ClusterIP. The opt-in federation route is the sole exact public A2A exception; kagent remains private behind NetworkPolicy.
1. Delegation uses the official `a2a-go` SDK and non-streaming `SendMessage`, with `GetTask` polling for long tasks. Do not hand-roll JSON-RPC. Remote routes receive no local `A2A_API_KEY` and require a currently verified, pinned ES256 Signed AgentCard plus reviewed transport authorization.
1. Agent ghosts are `@agent-<name>:<server>` in the bridge's exclusive namespace. Map each ghost to exactly one allowlisted local or remote target. Keep a separate `contextId` per `(room, ghost)`; retrieval-capable initial delegations omit caller context/task references.
1. Treat Matrix content as untrusted input. Sender policy gates invocation; agent replies are `m.notice` and must not trigger automation. `X-User-Id` is asserted attribution, not global authentication. No agent holds a model credential.
1. Agent rooms are unencrypted by policy. Same-organization behavior is governed by ADR 0008; ADR 0015's private, invite-only, joined-history, visibly plaintext, classification-bounded contract governs v1 partner rooms. An E2EE requirement blocks deployment until the crypto escape hatch exists.
1. Cost and resource efficiency are correctness constraints. Preserve per-sender/per-room limits and token metering. Cross-org `maxTokens` is a per-`azp` admission reservation, not measured consumption; never label it as spend.
1. Secrets are SOPS-age encrypted and cluster-specific. Commit only encrypted `*.sops.yaml` files or example templates; never plaintext credentials.
1. Coupled version pins move together. Gateway API, agentgateway, and Traefik compatibility is one deliberate upgrade boundary; kagent charts use an OCI-type `HelmRepository`. See the exact current pins in [docs/agent-reference.md](../docs/agent-reference.md).
1. Bridge images are multi-arch distroless GHCR artifacts deployed by immutable digest, never `latest`.
1. Project code is Apache-2.0. Keep bridge dependencies permissive (`mautrix/go` is MPL-2.0), `NOTICE` files current, and AGPL images in upstream registries. Contributions use DCO, not a CLA.

## Coding and operational conventions

- Go and Python are the core languages; Go is the bridge default. Prefer small typed units, fail fast at boundaries, wrap Go errors with `%w`, never ignore `err`, and keep deadlines on I/O.
- `mise` is the task source of truth; lefthook and CI reuse it. dprint formats markup/config. Conventional Commits carry no AI attribution or co-author trailer.
- The reference homeserver is ESS Community; Tuwunel/continuwuity is the permissive fallback. Switching follows [docs/licensing.md](../docs/licensing.md) §10.3, not per-PR preference.
- Reference domains are `fgentic.fmind.ai` and `fgentic.localhost`. The tracked default model is `google/gemini-2.5-flash` through agentgateway; `llm_provider=vllm` selects the self-hosted profile. Exact service names and endpoints are in [docs/agent-reference.md](../docs/agent-reference.md).
- `main` is protected against force pushes, deletion, and non-linear history. Agents still use issue → branch → PR → peer review by convention; see [.agents/skills/github-flow/SKILL.md](skills/github-flow/SKILL.md).
- Never weaken an assertion, add a skip, suppress a finding, loosen a type, or misstate runtime evidence to make a gate green. Fix the root cause and report any remaining proof boundary honestly.
