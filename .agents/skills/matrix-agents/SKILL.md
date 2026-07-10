---
name: matrix-agents
description: Runbooks for the Fgentic platform — bootstrap the Matrix + agent stack, register the A2A bridge, add an agent, add an external-network bridge, DNS/TLS, and verify the @mention->A2A->reply flow. Use when operating or extending the platform.
metadata:
  author: Médéric Hurier (Fmind)
  created: 2026-07-08
---

# Fgentic Runbook

An open-standard AI-agent collaboration platform: humans + agents share Matrix rooms and `@mention` to delegate over A2A. Layers: Matrix (ESS: Synapse + MAS + Element), the `matrix-a2a-bridge` (Go appservice), agentgateway (governed egress), kagent (agents). CD is Flux v2 pull-based; secrets are SOPS-age. See [PLAN.md](../../../PLAN.md).

## Golden rules

1. Never `kubectl apply` / `helm upgrade` prod by hand — **commit to git, let Flux reconcile**.
1. Never commit a plaintext secret — only `*.sops.yaml`. A gitleaks pre-commit hook enforces it.
1. The appservice registration (`as_token`/`hs_token`) must be **identical** in the bridge and in Synapse — one SOPS Secret, referenced from both namespaces.
1. Agent rooms are **unencrypted** by design (force-disabled server-side). Do not enable E2EE on agent rooms.

## Runbook: one-time bootstrap

1. **(Optional) Provision a cluster** — `cd infra/terraform && cp terraform.tfvars.example terraform.tfvars` (set your `/32`), then `terraform init && terraform apply`. Or use any conformant cluster / local k3d (`mise run cluster:up`).
1. **Gateway API CRDs** (the one out-of-band install): `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.0/experimental-install.yaml`.
1. **Create the secrets** — `scripts/gen-secrets.sh <server_name> <local|gcp>` writes the full consistent SOPS set (Postgres roles, registration tokens, connection URLs) into `clusters/<env>/secrets/`; commit + push (Flux applies from git).
1. **SOPS-age key**: `kubectl -n flux-system create secret generic sops-age --from-file=age.agekey="$HOME/.config/sops/age/keys.txt"` (create the namespace first if bootstrapping later).
1. **Local TLS (k3d only)**: `scripts/local-ca.sh` — generates + loads the `local-ca` CA secret (ESS bakes https URLs, so even local runs terminate real TLS at the Gateway on loopback 80/443).
1. **Bootstrap Flux**: `GITHUB_TOKEN=$(gh auth token) flux bootstrap github --owner=fmind-ai --repository=fgentic --path=clusters/<env>` — commits the flux-system manifests and starts reconciling.
1. **DNS A records (gcp)** — point `fgentic.fmind.ai`, `chat.`, `matrix.`, `auth.` at the ingress IP (`terraform output -raw ingress_ip`); cert-manager then issues the multi-SAN Let's Encrypt cert on `fgentic-gateway`.
1. Flux reconciles in order: secrets → controllers (cert-manager, Traefik, CNPG, agentgateway, kagent) → gateway (TLS) → postgres (databases/roles) → matrix (ESS) → agentgateway (LLM + A2A routes) → kagent (ModelConfig + sample Agent) → the bridge.

## Runbook: add an agent

1. **Declare the agent** — add a kagent `Agent` (with `a2aConfig`) in `infra/kagent/` referencing the `agentgateway-claude` ModelConfig; commit. kagent serves it over A2A at `…/api/a2a/kagent/<name>` with an AgentCard.
1. **Map a ghost** — add `agent-<name>: {namespace: kagent, name: <name>}` to the bridge's `agents` map (chart `values.yaml` in `clusters/base/apps.yaml` or `apps/matrix-a2a-bridge/chart/values.yaml`); commit. The ghost `@agent-<name>:fgentic.fmind.ai` becomes invokable (the map is the allowlist).
1. **Use it** — invite `@agent-<name>` into a room and `@mention` it.

## Runbook: add an external-network bridge (interop)

1. Deploy an off-the-shelf **mautrix** bridge (e.g. `mautrix-telegram`, `-signal`, `-slack`) as its own appservice in the `bridges` namespace, with its **own** Postgres database/role (add one to `infra/postgres`) and its own registration Secret.
1. Register it with Synapse (ESS appservice config) — its namespace (e.g. `@telegram_.*`) is disjoint from the agent ghosts, so they coexist.
1. Now an agent in a room can transparently talk to a bridged Telegram/Slack user. Start with clean-ToS networks; defer WhatsApp/Meta (ToS + phone dependency). See [ADR 0002](../../../docs/adr/0002-matrix-collaboration-fabric.md).

## Runbook: verify the flow

1. `kubectl -n bridge logs deploy/matrix-a2a-bridge` shows "matrix-a2a-bridge started" and the loaded agent map.
1. Prototype A2A directly: `go install github.com/a2aproject/a2a-go/v2/cmd/a2a@latest`, then port-forward agentgateway and run `a2a discover http://localhost:8080/api/a2a/kagent/platform-assistant` and `a2a send … "hello"`.
1. In Element: create a room, invite `@agent-assistant:fgentic.fmind.ai`, and post `@agent-assistant summarize this room`. The ghost replies in-thread.

## Runbook: cost / scale levers

Unlike the sibling `dev.fmind` (a $30/mo free-tier cluster), this platform is sized for the full stack. To trim: reduce kagent agents, run Synapse/MAS at one replica, or `terraform destroy` the reference cluster. To scale up: raise `node_count`/`machine_type`, set CNPG `instances: 3` for HA, and add read replicas.
