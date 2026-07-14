---
name: local-cluster
description: Run and troubleshoot Fgentic's repo-owned k3d profiles — lightweight development, full local GitOps, local CA/ADC prerequisites, and constrained-host quirks. Use when a local cluster is down, broken, or being recreated.
metadata:
  author: Médéric Hurier (Fmind)
  created: 2026-07-11
---

# Local Cluster (k3d)

The default development path is the owned lightweight `fgentic-demo` cluster. The full production-shaped platform remains available as `fgentic`; both bind loopback 80/443 to keep canonical `https://*.fgentic.localhost` URLs, so run only one at a time. First-time full-platform setup is the matrix-agents bootstrap runbook; this skill covers profile choice, lifecycle, and diagnosis.

## Choose the smallest sufficient profile

1. **No platform cluster:** focused unit tests first; use `mise run test:integration` for the isolated kind-based Matrix↔A2A boundary.
1. **Daily interactive bridge loop:** `mise run dev:up` creates/seeds the lightweight `fgentic-demo` once or starts it without reconciliation. Use `dev:reload` or `watch` for bridge-only changes, `dev:status` to inspect, `dev:stop` to release active CPU/RAM while preserving state, and `dev:down` to delete only the owned cluster. Every command generates a temporary kubeconfig and leaves the user's default context untouched.
1. **Manifest/profile acceptance:** `mise run demo:up` reconciles a fresh local snapshot and repeats admission plus seeded end-to-end proof. It retains gateway, Postgres, ESS, agentgateway, the kagent controller/tools and three mapped Agents, and the bridge; it omits Keycloak, observability, and Trivy, scales kagent UI to zero, and disables KMCP.
1. **Full local GitOps:** `mise run cluster:up` / `cluster:down` manages the production-shaped `fgentic` cluster from `infra/k3d-config.yaml`. Use it only for Keycloak SSO, telemetry/tracing, Trivy, SOPS, or full Flux bootstrap behavior omitted from demo. On a constrained laptop, `mise exec -- k3d cluster stop fgentic` releases active CPU/RAM without deleting state and `mise exec -- k3d cluster start fgentic` restores it.
1. A stopped node retains its original k3s command: `k3d cluster start` cannot adopt new server flags. If `docker inspect k3d-fgentic-server-0 --format '{{json .Config.Cmd}}'` lacks `--disable-network-policy`, keep that cluster stopped until its state can be discarded or migrated deliberately, then recreate it with `mise run cluster:down` followed by `mise run cluster:up`. Do not mutate host iptables or Docker daemon settings to retrofit it.
1. After a full-cluster recreate, redo the out-of-band steps: Gateway API CRDs, the `sops-age` Secret, `scripts/local-ca.sh`, `scripts/local-adc.sh`, then `flux bootstrap github … --path=clusters/local` (order and commands in the matrix-agents runbook). Once Flux is bootstrapped, run **`mise run cluster:overrides`** to re-apply the gitignored `platform-settings-overrides` (real `gcp_project`, etc.) — it is untracked, so a recreate loses it and Vertex falls back to the `your-gcp-project` placeholder until you do (idempotent; safe no-op if you never created the file).
1. The only prerequisites common to the repo-owned profiles are Docker, Git, and `mise install`. Docker Engine on Linux and Docker Desktop/compatible Docker daemons on macOS are supported; k3d auto-selects its local or remote-daemon image import mode. No global k3d cluster or `~/.agents/skills/k8s-local` setup is required.

## Diagnose top-down (symptom → layer)

1. **Everything**: `flux get kustomizations` — the first non-Ready Kustomization in the DAG is usually the root cause (dependents just wait). Then `flux get helmreleases -A`. Deep-dive per the flux-gitops skill.
1. **Browser cannot reach `chat.fgentic.localhost`**: is Traefik's LoadBalancer bound? `kubectl -n traefik get svc`; `curl -vk https://chat.fgentic.localhost/` from the host. TLS warnings mean the local CA is not trusted — rerun `scripts/local-ca.sh`.
1. **Login/auth broken**: MAS owns auth (MSC3861) — `kubectl -n matrix logs deploy/ess-matrix-authentication-service`; password login needs `Content-Type: application/json` on the compat endpoint.
1. **Mention gets no reply**: bridge logs (`kubectl -n bridge logs deploy/matrix-a2a-bridge`) → agentgateway (`kubectl -n agentgateway-system logs deploy/agentgateway`) → kagent controller logs, in that order; the matrix-agents verify runbook has the isolation probes (AgentCard fetch, raw `POST /v1/chat/completions`).
1. **LLM calls fail locally**: Vertex AI auth is the ADC Secret (`gcp-adc` in `agentgateway-system`) — expired/missing ADC is the usual cause; rerun `scripts/local-adc.sh`. A second cause after a recreate: the real `gcp_project` lives in the untracked `platform-settings-overrides` (committed value is the `your-gcp-project` placeholder) — if Vertex rejects the project, run `mise run cluster:overrides`.
1. **NetworkPolicy "bugs"**: repo-owned k3d servers disable K3s's embedded kube-router controller because this constrained rootless/userns host aborts its full `iptables-restore` with `sendmsg() failed: Message too large`; a deny-all egress probe remained open and no `KUBE-POD-FW` chains survived. Policies are intent-only locally; prove enforcement with `mise run test:network-policies:kind` and on GKE Dataplane V2. Kube-router failure logs mean the node predates the flag: stop it and schedule a deliberate recreate.

## Constrained hosts (rootless Docker, ChromeOS crostini)

Already encoded in the repo — know why before removing them:

1. `infra/k3d-config.yaml` k3s/kubelet/kube-proxy flags: `--disable-network-policy` stops a controller this host cannot enforce; `KubeletInUserNamespace` + `fail-cgroupv1=false` handle kernel flags in userns; and `masquerade-all=true` handles missing `br_netfilter` (same-node pod→Service replies otherwise bypass un-DNAT and time out).
1. `clusters/local/flux-system/kustomization.yaml` patches lenient leader-election leases onto the Flux controllers (high host load avg makes the API server miss lease renewals → controllers crash-loop). Under load, expect slowness and prefer generous timeouts over restarts.
1. Run `mise run check` and `mise run test` **one at a time**, never concurrently. Both are heavy (~5–9 min each here); in parallel they starve each other and fail spuriously — `check:scan` (Trivy) hits `context deadline exceeded` and `check:app`/`check:gateway` golangci-lint dies with "no exit status" (SIGKILL under memory pressure). Each passes cleanly in isolation; CI runs them sequentially, so it is unaffected.
