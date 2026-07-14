---
name: local-cluster
description: Run and troubleshoot the local k3d platform — cluster lifecycle, local CA/ADC prerequisites, layer-by-layer diagnosis when fgentic.localhost misbehaves, and constrained-host (rootless/crostini) quirks. Use when the local cluster is down, broken, or being (re)created.
metadata:
  author: Médéric Hurier (Fmind)
  created: 2026-07-11
---

# Local Cluster (k3d)

The full platform runs on one k3d cluster (`fgentic`) with loopback 80/443 bound to the Gateway, so everything lives at its canonical `https://*.fgentic.localhost` URL (ESS bakes https URLs — no port suffixes). First-time setup is the matrix-agents bootstrap runbook; this skill is lifecycle + diagnosis.

## Lifecycle

1. `mise run cluster:up` / `mise run cluster:down` (config: `infra/k3d-config.yaml` — k3s Traefik disabled, we install our own). Requires a running Docker daemon.
1. After a recreate, redo the out-of-band steps: Gateway API CRDs, the `sops-age` Secret, `scripts/local-ca.sh`, `scripts/local-adc.sh`, then `flux bootstrap github … --path=clusters/local` (order and commands in the matrix-agents runbook). Once Flux is bootstrapped, run **`mise run cluster:overrides`** to re-apply the gitignored `platform-settings-overrides` (real `gcp_project`, etc.) — it is untracked, so a recreate loses it and Vertex falls back to the `your-gcp-project` placeholder until you do (idempotent; safe no-op if you never created the file).
1. Rebuild-and-run a bridge change: `mise run bridge:load` + rollout restart, or `mise run watch` — see the bridge-dev skill.

## Diagnose top-down (symptom → layer)

1. **Everything**: `flux get kustomizations` — the first non-Ready Kustomization in the DAG is usually the root cause (dependents just wait). Then `flux get helmreleases -A`. Deep-dive per the flux-gitops skill.
1. **Browser cannot reach `chat.fgentic.localhost`**: is Traefik's LoadBalancer bound? `kubectl -n traefik get svc`; `curl -vk https://chat.fgentic.localhost/` from the host. TLS warnings mean the local CA is not trusted — rerun `scripts/local-ca.sh`.
1. **Login/auth broken**: MAS owns auth (MSC3861) — `kubectl -n matrix logs deploy/ess-matrix-authentication-service`; password login needs `Content-Type: application/json` on the compat endpoint.
1. **Mention gets no reply**: bridge logs (`kubectl -n bridge logs deploy/matrix-a2a-bridge`) → agentgateway (`kubectl -n agentgateway-system logs deploy/agentgateway`) → kagent controller logs, in that order; the matrix-agents verify runbook has the isolation probes (AgentCard fetch, raw `POST /v1/chat/completions`).
1. **LLM calls fail locally**: Vertex AI auth is the ADC Secret (`gcp-adc` in `agentgateway-system`) — expired/missing ADC is the usual cause; rerun `scripts/local-adc.sh`. A second cause after a recreate: the real `gcp_project` lives in the untracked `platform-settings-overrides` (committed value is the `your-gcp-project` placeholder) — if Vertex rejects the project, run `mise run cluster:overrides`.
1. **NetworkPolicy "bugs"**: K3s includes kube-router's NetworkPolicy controller even with Flannel, but this constrained rootless/userns host aborts its full `iptables-restore` with `sendmsg() failed: Message too large`; a deny-all egress probe remained open and no `KUBE-POD-FW` chains survived. Treat policies as intent-only locally until that probe passes; prove security enforcement on GKE Dataplane V2 or another verified cluster.

## Constrained hosts (rootless Docker, ChromeOS crostini)

Already encoded in the repo — know why before removing them:

1. `infra/k3d-config.yaml` kubelet/kube-proxy flags: `KubeletInUserNamespace` + `fail-cgroupv1=false` (kubelet cannot write kernel flags in userns) and `masquerade-all=true` (no `br_netfilter` → same-node pod→Service replies bypass un-DNAT and time out; pod→pod works, Service IPs don't — the classic symptom).
1. `clusters/local/flux-system/kustomization.yaml` patches lenient leader-election leases onto the Flux controllers (high host load avg makes the API server miss lease renewals → controllers crash-loop). Under load, expect slowness and prefer generous timeouts over restarts.
1. Run `mise run check` and `mise run test` **one at a time**, never concurrently. Both are heavy (~5–9 min each here); in parallel they starve each other and fail spuriously — `check:scan` (Trivy) hits `context deadline exceeded` and `check:app`/`check:gateway` golangci-lint dies with "no exit status" (SIGKILL under memory pressure). Each passes cleanly in isolation; CI runs them sequentially, so it is unaffected.
