---
name: flux-gitops
description: Change and debug the Fgentic platform via Flux GitOps — the Kustomization DAG, per-cluster overlays and platform-settings substitution, manifest validation, reconciliation debugging, and the version pins that bind each other. Use for any change under infra/ or clusters/, or when the cluster diverges from git.
metadata:
  author: Médéric Hurier (Fmind)
  created: 2026-07-11
---

# Fgentic Flux GitOps

Delivery is Flux v2, pull-based. **Never `kubectl apply` / `helm upgrade` by hand** — commit to git, let Flux reconcile (`flux reconcile ks flux-system --with-source` to skip the poll interval). `kubectl` is for reading and debugging only.

## The DAG (clusters/base)

`clusters/base/infrastructure.yaml` + `apps.yaml` define Flux Kustomizations, one per `infra/` directory, ordered by `dependsOn`:

`namespaces` → `platform-secrets` + `controllers` (infra/flux) + `observability` → `gateway`, `postgres`, `agentgateway` → `matrix` (ESS), `kagent`, `observability-monitors` → `bridge` (apps/matrix-a2a-bridge/deploy).

1. `namespaces` is dependency-free and owns **every** Namespace + PSS labels — HelmReleases/Secrets cannot land in namespaces that don't exist, and per-layer namespaces deadlock the DAG. New namespace ⇒ add it to `infra/namespaces/`.
1. `HelmRelease.dependsOn` may only reference other HelmReleases — to depend on anything else, wrap in a Flux Kustomization and use its `dependsOn` (that's why apps get their own Kustomization).
1. Adding a layer: create `infra/<name>/` with a `kustomization.yaml` listing resources, add a Flux Kustomization to `clusters/base/` with correct `dependsOn`, commit.

## Per-cluster configuration

1. Environment values live in ONE place: the `platform-settings` ConfigMap (`clusters/<env>/platform-settings.yaml` — server_name, cluster_issuer, gcp_project, llm_model, …), injected into every manifest by Flux **postBuild substitution** (`${server_name}` style). Manifests carry no environment specifics. Ad-hoc override without editing git: create a `platform-settings-overrides` ConfigMap in `flux-system`.
1. Structural differences are patches in `clusters/<env>/kustomization.yaml` (e.g. local strips CNPG GCS backups, swaps the bridge image for the side-loaded one, points agentgateway at the ADC secret).
1. Real SOPS secrets are per-cluster in `clusters/<env>/secrets/` — see the `sops-secrets` skill.

## Validate before committing

1. `mise run check` runs everything CI runs: dprint, terraform validate, **kubeconform** (`scripts/kubeconform.sh` — schema-validates the chart render + raw manifests), **trivy config**, gitleaks, actionlint.
1. To preview a Helm render locally: `helm template <release> <chart> -f <values>` — remember Flux substitution variables stay unexpanded outside the cluster.

## Debug reconciliation

1. Status sweep: `flux get kustomizations` then `flux get helmreleases -A`; details: `kubectl describe ks <name> -n flux-system` / `flux logs --level=error`.
1. A HelmRelease stuck in a failed upgrade: `flux reconcile hr <name> -n <ns> --force`; if retries are exhausted, `flux suspend hr <name> -n <ns> && flux resume hr <name> -n <ns>`.
1. Secrets not decrypting: the `sops-age` Secret in `flux-system` must exist (bootstrap step).
1. ESS chart values are schema-validated by the chart and the schema **changes between CalVer releases** — a values key that worked can fail after a chart bump; read the release notes.

## Version pins that bind each other (bump deliberately, together)

1. Gateway API CRDs **v1.4.0 experimental** ↔ agentgateway v1.3.1 (watches TCPRoute v1alpha2, removed in v1.6) ↔ traefik chart **39.x** (proxy v3.6; v3.7 expects Gateway API v1.6). CRDs are the one out-of-band install (see the matrix-agents bootstrap runbook).
1. kagent charts come via an **OCI-type HelmRepository**, never `chartRef` → OCIRepository (Flux appends digest build-metadata that kagent stamps into a label, which breaks the release).
1. The bridge deploys by **immutable digest** written by CD — the local overlay swaps it for the side-loaded `matrix-a2a-bridge:local` image.

## Non-negotiables

1. Web/UI traffic uses Gateway API HTTPRoutes through the shared `fgentic-gateway`; agent traffic (A2A/LLM) egresses only through agentgateway on ClusterIP. The NetworkPolicies on the agentgateway **and** kagent namespaces are load-bearing (kagent's A2A endpoint is unauthenticated) — never remove or loosen them.
1. Every mention is an LLM invocation: keep the bridge rate limits and agentgateway token metering intact (D7/D8).
1. Never mirror AGPL images (Synapse, Grafana) into project registries — reference upstream.
