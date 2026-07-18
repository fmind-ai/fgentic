---
type: Runbook
title: Production Installation
description: Production path: Flux reconciliation of a reviewed revision, SOPS secrets, SSO, observability, and acceptance gates.
---

# Production Installation

The production path reconciles a reviewed git revision through Flux, decrypts per-cluster SOPS secrets in-cluster, enables SSO and observability, and keeps the canonical HelmRelease values under `infra/` and `apps/`. It is intentionally different from the disposable [evaluation installer](../README.md#evaluate-in-15-minutes). After bootstrap, use the [Day-2 Operations Handbook](operations-handbook.md) for monitoring, scaling, recovery, incident response, and upgrades.

## Choose the model boundary

Choose where prompts and responses may travel before generating secrets. The exact settings, credential names, network paths, and acceptance gates are in [models.md](models.md).

| Tier | Profiles                        | Boundary                                                                                                                  |
| ---- | ------------------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| 1    | `vllm`                          | Self-hosted serving; prompts stay in the cluster after the pinned model bootstrap, subject to verified NetworkPolicy      |
| 2    | `mistral`                       | EU API endpoint; contract, subprocessors, retention, and billing remain account controls                                  |
| 3    | `vertex`, `anthropic`, `openai` | Hyperscaler boundary; region/residency and retention depend on provider and account configuration                         |
| 3    | `azure-openai`                  | Azure resource boundary; select Regional or EU Data Zone rather than Global when geography must be constrained            |
| —    | `demo` evaluation fixture       | Not a language model and not supported by the `local` or `gcp` production overlays; never use it for a production install |

The tracked `local` and `gcp` references select `vertex` / `google/gemini-2.5-flash`. Change `llm_provider` and `llm_model` in `clusters/<env>/platform-settings.yaml` when a different boundary is required. API-key profiles require their documented environment variable when secrets are generated. Vertex uses a cluster-only ADC Secret on k3d; Terraform grants the exact GKE agentgateway proxy Workload Identity direct Vertex access without a Google service account or key. Live GKE acceptance remains spend-gated in [#59](https://github.com/fmind-ai/fgentic/issues/59). In every profile, the model credential terminates at agentgateway rather than an Agent.

## Prerequisites

- A conformant Kubernetes cluster, or Docker for the local k3d reference.
- Git, [mise](https://mise.jdx.dev/), and the repository checkout. `mise install` installs the pinned `kubectl`, Flux, k3d, SOPS, age, Helm, and validation tools.
- A writable GitHub repository and token for the current `flux bootstrap github` reference workflow. Flux itself is provider-neutral; adapt its source bootstrap when using another Git host.
- An age private key whose recipient matches [.sops.yaml](../.sops.yaml).
- Provider credentials only for the selected model profile.

For the optional GKE reference, apply [`infra/terraform/bootstrap/`](../infra/terraform/bootstrap/) first to create the versioned state bucket, migrate the main Terraform state, review the plan and cost, and apply [`infra/terraform/`](../infra/terraform/) only with maintainer approval. The workloads remain portable Kubernetes manifests.

## Bootstrap order

1. Install the pinned tools and hooks:

   ```bash
   mise install
   mise run install:hooks
   ```

1. Create or select the cluster. For the local reference:

   ```bash
   mise run cluster:up
   ```

1. Install the pinned experimental Gateway API v1.4.0 CRDs. This is the only out-of-band CRD bundle:

   ```bash
   kubectl apply --server-side \
     -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/experimental-install.yaml
   ```

1. Set the chosen `llm_provider` and `llm_model` in `clusters/<env>/platform-settings.yaml`, export the selected API key if applicable, and generate the complete encrypted secret set:

   ```bash
   scripts/gen-secrets.sh <server_name> <local|gcp>
   ```

   Review the SOPS resources under `clusters/<env>/secrets/`, then commit and push them. Never commit plaintext credentials. The generator keeps the Matrix appservice tokens identical across the Matrix and bridge namespaces and scopes each database role to one service.

1. Install the decryption key in the cluster. Create the namespace first only when Flux bootstrap has not done so:

   ```bash
   kubectl get namespace flux-system >/dev/null 2>&1 || kubectl create namespace flux-system
   kubectl -n flux-system create secret generic sops-age \
     --from-file=age.agekey="$HOME/.config/sops/age/keys.txt"
   ```

1. For local k3d, create the local CA Secret and follow the printed host-trust instruction:

   ```bash
   scripts/local-ca.sh
   ```

1. For local Vertex only, create its cluster-only ADC Secret. API profiles use their generated SOPS Secret; vLLM uses no model credential:

   ```bash
   scripts/local-adc.sh <gcp-project>
   ```

1. Bootstrap Flux against the reviewed repository and cluster overlay:

   ```bash
   flux bootstrap github \
     --owner=<owner> \
     --repository=fgentic \
     --path=clusters/<local|gcp>
   ```

1. Local k3d cannot pull the private development bridge image, so build and side-load it. Production CD publishes, signs, and digest-pins the image instead:

   ```bash
   mise run bridge:load
   ```

Flux reconciles the DAG in dependency order: namespaces and secrets; controllers and observability; gateway, Postgres, and agentgateway; Matrix, Keycloak, kagent, and monitors; then the bridge. Inspect `flux get kustomizations` and `flux get helmreleases -A`; debug the first non-Ready layer instead of applying a workload around Flux.

## Availability posture

The GCP reference explicitly composes [`infra/production-ha/cluster`](../infra/production-ha/cluster); `local`, `demo`, and `federation` do not. The profile replicates only workloads whose current state and protocol boundaries support it. A PodDisruptionBudget protects against voluntary eviction; it does not keep a process alive through node, zone, storage, control-plane, or application failure. Likewise, `DoNotSchedule` hostname spreading needs at least two schedulable nodes with enough capacity before the second replica can start.

The default Terraform reference has two nodes in one zone. It therefore demonstrates host-level separation, not zone or region availability. Setting `regional = true` changes the GKE control-plane and node-pool topology, but operators must still verify workload placement, storage topology, capacity, and failure behavior across the intended zones; a regional control plane alone is not that proof.

| Component                                | Production profile                                                              | Honest boundary                                                                                                            |
| ---------------------------------------- | ------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| CNPG `platform-pg`                       | 3 instances; CNPG-owned failover and disruption budget                          | Database process/node resilience; backup and restore remain separate gates                                                 |
| Traefik                                  | 2 replicas, hostname spread, one-pod PDB                                        | One voluntary disruption or host loss; not zone HA                                                                         |
| Element Web, HAProxy, MAS                | 2 replicas each, hostname spread, one-pod PDBs                                  | Stateless web, routing, and authentication edge redundancy                                                                 |
| Synapse main                             | 1 replica, no PDB; retained RWO media PVC; GKE CSI snapshot class               | Fast restart only; [ADR 0019](adr/0019-synapse-media-store.md) requires a coordinated restore drill before recovery claims |
| Keycloak                                 | 2 replicas, JDBC discovery, required host anti-affinity, one-pod PDB            | Active/active application processes against shared Postgres                                                                |
| agentgateway controller and proxy        | 2 replicas each, required host anti-affinity, zero-surge rollouts, one-pod PDBs | Controller and data-plane pod redundancy                                                                                   |
| MCP quota service and persistent store   | 2 service replicas with host anti-affinity and one-pod PDB; 1 RWO store, no PDB | The policy hop survives one voluntary disruption; MCP tool calls fail closed while the store restarts or reattaches        |
| kagent controller, KMCP, tools, and UI   | 2 replicas each, hostname spread, one-pod PDBs                                  | Platform workloads only; generated Agent workloads remain one replica                                                      |
| Matrix-to-A2A bridge                     | 1 ready intake replica, no PDB, zero-surge rollout                              | Postgres-backed work recovery and cross-restart room ordering; intake is unavailable while the single replica restarts     |
| Other operators and observability stores | Existing chart defaults                                                         | Fast restart; Jaeger's in-memory trace store is explicitly ephemeral                                                       |

The static contract recursively renders the effective GCP and local Flux trees, renders the pinned third-party charts, verifies replica/PDB/resource and placement-selector invariants, and proves the production component did not leak into evaluation profiles:

```bash
mise run check:production-ha
```

The bridge availability fixture then holds one real A2A call, normally deletes the bridge pod, requires the old process to observe SIGTERM and deliver one reply, observes a distinct replacement process, and directly replays the same transaction body to require no second A2A call or reply:

```bash
mise run test:availability
```

This remains a graceful-drain test, not an automatic Synapse-retry or SIGKILL test. The separate crash fixture runs real Postgres, Synapse, A2A, and bridge processes and SIGKILLs the bridge at twelve persisted job/control boundaries:

```bash
mise run test:crash-recovery
```

The boundaries are ledger/control commit before acknowledgement, acknowledged work before claim, A2A send/cancel/continuation acceptance before its record, persisted result/control projection before Matrix, Matrix acceptance before event-ID record, a known task blocked in `GetTask`, and restart before terminal cleanup. Run the task on the candidate revision before claiming those workflows; its total scenario duration is diagnostic only and is not a recovery-time assertion. The bridge commits the exact appservice transaction hash and all eligible per-target jobs/controls before HTTP 200, fences recovery with expiring generation leases, and preserves database-enforced per-room FIFO across a replacement process. Recovered work follows protocol state: unstarted jobs run, known task IDs resume through `GetTask`, pending Matrix replies/questions/progress/pin state reuse deterministic transaction IDs, and a bounded `awaiting_input` window survives replacement. A `SendMessage`, `CancelTask`, or continuation whose HTTP transport may have started but returned no acknowledgement is instead terminal `ambiguous` and is never resent, because its deterministic A2A identity does not prove target idempotency. Neither local fixture simulates node loss or establishes a production RTO/SLO. Re-run the graceful drill on the target cluster before setting either.

On 2026-07-14, an owned one-node Kind v1.34.0 fixture on the cgroup-v1 local development host observed `delivery_gap_ms=1149`, `replacement_observed_ms=11563`, and `pod_ready_rto_seconds=2`. The normally deleted pod was `bridge-6c64649f68-9667x`; the distinct ready replacement was `bridge-6c64649f68-cmcvd`. The content-free counters remained exactly one A2A start, one completion, one Matrix reply, and one replay suppression across the quiet window. These values characterize that graceful fixture run only; they are not crash-recovery timings.

## Bound namespace compute

The namespaces layer creates a `compute-budget` ResourceQuota and `container-defaults` LimitRange before any application workload. The quota caps aggregate Pods and CPU/memory requests and limits; it does not reserve that capacity. The LimitRange adds a 25m/64Mi request and 500m/512Mi limit only where a container omits the corresponding field, so reviewed workload-specific values remain authoritative and every admitted Pod is accounted.

Namespaces use three coarse profiles rather than one independently tuned setting per component:

| Profile   | Namespaces                                                                                                                                                                   | Local/demo/federation: pods · requests · limits | GCP reference: pods · requests · limits |
| --------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------- | --------------------------------------- |
| `small`   | `cert-manager`, `gateway`, `cnpg-system`, `postgres`, `knowledge`, `keycloak`, `agentgateway-system`, `bridge`, `bridges`, `trivy-system`; dormant `activitypub` deploy unit | 12 · 2 CPU/4 GiB · 8 CPU/8 GiB                  | 24 · 4 CPU/8 GiB · 12 CPU/16 GiB        |
| `core`    | `matrix`, `kagent`, `monitoring`; federation-only `matrix-b`, `matrix-c`                                                                                                     | 24 · 4 CPU/8 GiB · 16 CPU/16 GiB                | 32 · 8 CPU/16 GiB · 24 CPU/24 GiB       |
| `compute` | `models`                                                                                                                                                                     | 8 · 4 CPU/8 GiB · 8 CPU/12 GiB                  | 16 · 8 CPU/16 GiB · 16 CPU/24 GiB       |

All 17 repository-managed Namespace manifests carry one compute quota and one LimitRange: 14 shared namespaces, the federation-only `matrix-b` and `matrix-c`, and the dormant self-contained `activitypub` deploy unit. The scanner layer adds a second, narrow `count/jobs.batch: 1` quota in `trivy-system` because Kubernetes admission—not the operator's racy informer cache—must serialize scan Jobs. The effective demo removes `trivy-system`; the federation component omits `bridge`, `bridges`, `monitoring`, and `trivy-system` together with their admission objects, then adds both secondary homeservers, so its reconciled sets remain exactly aligned at 12 Namespaces, compute quotas, and LimitRanges. The bootstrap-critical cert-manager and CNPG operators retain complete explicit resources plus generous `small` rollout headroom. Kubernetes and Flux system namespaces are outside these owned layers and remain unmodified.

The 2026-07-13 pinned-render audit found complete workload resources on kagent and the sample agents, the bridge and optional external bridges, model runtimes, and the primary Postgres, Keycloak, gateway, agentgateway, and observability containers. It also found deliberate or chart-owned gaps: ESS supplies requests and memory limits but no CPU limits; monitoring sidecars, exporters, operator hook Jobs, and some operator-generated helpers omit one or more fields. The namespace defaults cover those generated containers without duplicating fragile third-party chart internals. Cert-manager's four components and the agentgateway controller were corrected to explicit resources because their charts expose stable top-level values.

These are generous initial ceilings, not measured steady-state targets. Tighten only after capturing `kubectl describe quota -A` during normal traffic and a full rolling upgrade, retaining explicit rollout and failure-recovery headroom. Change the three profile values in `clusters/<env>/platform-settings.yaml`; do not patch individual ResourceQuota objects. Workload-level resource and availability invariants are enforced by `mise run check:production-ha`; namespace quotas complement them by bounding aggregate blast radius.

Run the offline mapping/substitution contract on every change and the negative admission proof on an isolated cluster:

```bash
mise run check:resource-quotas
mise run test:resource-quotas
```

The runtime test creates its own no-port kind cluster and kubeconfig, starts one agent Deployment, scales it past a two-Pod ceiling, and requires the ReplicaSet's `FailedCreate` event to name `compute-budget`. After a real Flux rollout, require every Kustomization and HelmRelease Ready, inspect `kubectl describe quota -A`, and repeat representative rollouts before treating static ceilings as target-cluster acceptance.

## Operate continuous vulnerability scanning

The production `local` and `gcp` overlays include the separate `trivy-operator` Flux layer; evaluation `demo` and disposable `federation` remove it structurally. Do not add a boolean setting that leaves dormant CRDs, RBAC, or scan Jobs in those smaller profiles. The immutable source of truth is [`infra/trivy-operator/`](../infra/trivy-operator/): Trivy Operator v0.32.0 and its chart 0.34.0 come from release commit `1006872c1463e81a40d48298145625aefef2a02f`, while both the operator and Trivy scanner images carry explicit SHA-256 digests recorded in [§9.9](observability.md#99-runtime-image-vulnerability-drift).

After Flux reconciliation, inspect the layer, release, reports, and scrape target without broadening report access:

```bash
flux get kustomization trivy-operator
flux get helmrelease trivy-operator -n trivy-system
kubectl get vulnerabilityreports.aquasecurity.github.io -A
kubectl -n monitoring get servicemonitor trivy-operator
```

The report list exposes package and CVE metadata to anyone allowed to read it. Grant triage access deliberately; the reference deletes the chart's aggregate report-view roles. Follow the [least-privilege and registry boundary](security.md#79-continuous-image-vulnerability-boundary) instead of enabling global Secret/ServiceAccount access. Private-registry credentials are unsupported on pinned v0.32.0 because quota rejection can orphan the generated credential Secret and suppress the retry; do not configure them until an upstream fix has a cleanup-and-eventual-report runtime case. A public registry or proxy on a non-443 port additionally needs a reviewed egress-policy exception and, when destination identity matters, an egress proxy or FQDN-aware CNI.

When `TrivyImageVulnerabilityDrift` fires:

1. Open the `VulnerabilityReport` for the alert's namespace, repository, digest, and severity. Confirm the newly reported package/CVE and whether an upstream fixed version exists; count growth alone does not prove exploitability.
1. Update the source dependency and immutable image digest in a reviewed PR. Do not edit the generated report, deploy a mutable tag, or silence the rule as remediation. Verify signatures, attestations, and SBOMs where the artifact publishes them, following the [supply-chain runbook](security/supply-chain.md).
1. Let Renovate propose routine pin updates after the maintainer completes [#6](https://github.com/fmind-ai/fgentic/issues/6), but keep human review and the full CI/runtime gates. Renovate availability is not an incident-response dependency; submit the digest-bump PR directly when a fix is urgent.
1. Reconcile the approved revision, confirm the new digest produces a fresh `VulnerabilityReport`, and verify the old workload is gone. A new digest intentionally starts a new alert baseline, so alert clearance alone is not proof that the named CVE was removed.

Static and rule checks are necessary but not runtime acceptance:

```bash
mise run check:trivy-operator
mise run check:prometheus
mise run test:trivy-operator
```

Before treating the layer as accepted, the runtime gate must use an ownership-labelled, no-port disposable k3d cluster; pre-occupy the scan quota, prove an operator scan Job is rejected, release the hold, and require schema-valid `VulnerabilityReport` objects for both simultaneously submitted digest-pinned fixtures without assuming a particular CVE. It must also prove the report metrics change, out-of-scope RBAC denials, one-at-a-time execution, and an observed operator working set below its 256 MiB limit. The gate must remove its cluster, containers, every named or anonymous volume, Docker network, kubeconfig, and image-volume artifacts on success and failure without targeting `local` or `fgentic`; only an explicitly configured diagnostics directory may remain after failure for inspection. No observed memory value is claimed here until that evidence exists.

The hosted nightly `scanner` job must repeat the same gate and preserve diagnostics on failure. Require that job, the Prometheus rule fixtures, and the aggregate smoke result to pass at the exact candidate revision before production reconciliation. On the installed cluster, additionally require the ServiceMonitor target to be up, a fresh report in a representative target namespace, and quota headroom during a scan.

## Provision the administrator and room

Run the supported interactive bootstrap after every layer is Ready:

```bash
scripts/bootstrap-admin.sh --server-name <server_name>
```

Open the one-time URL and authenticate as the IdP user whose immutable `matrix_localpart` is `alice`. The device grant provisions the exact Matrix ID, grants Synapse administrator access, and idempotently creates `#fgentic-demo:<server_name>` without storing a token or entering a pod. In Element at `https://chat.<server_name>`, send `!agents`, invite an allowed ghost, and mention it. Grafana is at `https://grafana.<server_name>`.

The complete identity contract is in [identity.md](identity.md); model runtime checks are in [models.md](models.md); attribution verification is in [audit.md](audit.md); secret rotation and mention-to-reply diagnostics are in the [matrix-agents runbook](../.agents/skills/matrix-agents/SKILL.md). The optional Ketesa administrator UI remains disabled until its [admin-console runbook](admin-console.md) and live admin/non-admin acceptance are completed.

Slack and Telegram are optional external identity/data boundaries, not production prerequisites. Enable them only through the composable cluster components and provider gates in [external-network interop](interop.md); the default local and GCP overlays reconcile neither bridge. Their standard NetworkPolicy permits arbitrary non-private IPv4 TCP/443 for provider transports, so deployments requiring provider-FQDN enforcement must add a governed egress proxy or FQDN-aware CNI before acceptance.

## Production gates

1. Run `mise run check` and `mise run test` warning-free before reconciling a revision.
1. Review [security.md](security.md), including prompt-injection boundaries, A2A workload authorization, network-policy enforcement, secret handling, and supply-chain verification.
1. Prove NetworkPolicy on GKE Dataplane V2 or another known-enforcing engine; repo-owned k3d servers deliberately disable the failed kube-router controller and are intent-only.
1. Confirm at least two schedulable nodes have replica and rollout headroom, then verify every required hostname-spread or anti-affinity rule places replicas on distinct nodes.
1. Inspect every PDB and perform a controlled target-cluster drain; a rendered PDB is not evidence that eviction, rescheduling, storage attachment, and application recovery succeed together.
1. Run `mise run test:availability` and `mise run test:crash-recovery` on the candidate revision; retain the graceful-drain timings and twelve-boundary SIGKILL result separately. Keep node loss and every process-recovery timing outside a claimed RTO until the corresponding target-environment drill passes.
1. Confirm selected-provider retention, residency, billing cap, and low-token runtime acceptance. Static rendering is not runtime evidence.
1. Configure DNS and valid TLS for the apex plus `chat.`, `matrix.`, `auth.`, `id.`, and `grafana.` hosts. Add `admin.` only when the Ketesa profile is enabled. The GKE Terraform output provides the reserved ingress address.
1. Review CNPG backups and complete a restore drill. The local overlay intentionally strips GCS backup configuration.
1. Verify signed, digest-pinned bridge artifacts and collect one end-to-end attribution bundle before declaring the deployment ready.

## Why evaluation still embeds Flux

The source of Helm values is the set of Flux `HelmRelease` resources. Applying those CRs to a cluster without helm-controller does not install their charts, while independently translating each one into `helm install` commands would create a second renderer and invite value drift. The evaluation command therefore installs local Flux controllers and reconciles an ephemeral cluster-local Git snapshot of the checkout. It needs no GitHub account, commit, push, SOPS key, or checkout mutation, while production and evaluation consume the same HelmReleases.

Evaluation deliberately diverges in lifecycle and hardening: its secrets are cluster-only, its Git source disappears with the cluster, it omits Keycloak and observability, and its default provider is a deterministic response stub. `mise run demo:down` deletes only `fgentic-demo`; production teardown follows the chosen infrastructure provider's reviewed, approval-gated process.
