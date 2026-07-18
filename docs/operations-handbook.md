---
type: Runbook
title: Day-2 Operations Handbook
description: Evidence-bound monitoring, scaling, recovery, incident, and upgrade procedures for a Flux-reconciled Fgentic deployment.
---

# Day-2 Operations Handbook

This handbook is the operator entrypoint after [Production Installation](production.md). It connects Fgentic's GitOps composition to the upstream components it assembles, while keeping three kinds of evidence separate:

1. **Declared posture** is what the reviewed Git revision and effective overlay render.
1. **Control evidence** is what the repository's static and isolated test gates prove.
1. **Operational evidence** is what the target cluster actually observed under its traffic, capacity, failure, restore, and upgrade conditions.

Do not promote one kind into another. Rendered replicas do not prove availability, a successful answer does not prove authorization, a backup object does not prove restoration, and a quota or token reservation is not measured consumption.

## 1. Establish the operating baseline

Record the following together for every production change and incident:

- Git revision, cluster overlay, effective platform settings, component/chart versions, and bridge image digest;
- first non-Ready Flux Kustomization or HelmRelease, affected namespace/workload, and event timestamps;
- relevant content-free metric windows, alert state, and the exact validation or recovery procedure run;
- owner, decision, rollback boundary, and any evidence that was not collected.

Flux owns production reconciliation. Inspect the dependency DAG before the workloads it creates:

```bash
flux get kustomizations
flux get helmreleases -A
```

Debug the first non-Ready layer rather than applying around it with `kubectl apply` or `helm upgrade`. The [Flux troubleshooting guide](https://fluxcd.io/flux/cheatsheets/troubleshooting/) is the component-level reference; [Production Installation](production.md) documents Fgentic's layer order and target-cluster acceptance gates.

## 2. Read the service signals

Grafana exposes the bridge and LLM dashboards at `grafana.<server_name>`. Prometheus scrapes the bridge, agentgateway, CloudNativePG, and the other configured monitors. Start from the symptom, then correlate the smallest relevant signals:

| Question                                                            | Primary signal                                                                                                            | Interpretation boundary                                                                                                          |
| ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| Are delegations completing?                                         | `fgentic_delegations_total{ghost,outcome}`                                                                                | Outcomes include success and explicit failure states; the counter is not a unique-human-task count.                              |
| Is bridge work accumulating?                                        | `fgentic_queue_depth`, `fgentic_inflight_delegations`                                                                     | A queue increase can mean downstream latency or insufficient admitted capacity; it is not permission to increase limits blindly. |
| Is durable recovery healthy?                                        | `fgentic_delegation_ledger_transitions_total{from_state,to_state}`, `fgentic_delegation_recovery_outcomes_total{outcome}` | `ambiguous` is a deliberate terminal safety result. Never resend it automatically.                                               |
| Are retries being suppressed?                                       | `fgentic_dedup_skips_total`                                                                                               | Correlate with Matrix transaction handling before calling repeated user messages duplicates.                                     |
| How long does initial delegation take?                              | `fgentic_a2a_request_seconds`                                                                                             | Measures `SendMessage`; long-task `GetTask` polling is outside this histogram.                                                   |
| How many model tokens were reported?                                | `agentgateway_gen_ai_client_token_usage_sum` by provider, model, route, and token type                                    | Provider-reported token telemetry is aggregate usage, not an invoice, per-user attribution, or a federation reservation.         |
| Is recent aggregate token burn above the configured warning budget? | `LLMTokenBurnHigh` and `system_model_token_type:agentgateway_gen_ai_client_token_usage_sum:increase15m`                   | The alert sums the 15-minute increase and waits 5 minutes. It is a warning detector, not a hard token or currency limit.         |

The LLM dashboard's catalog panel reports whether a provider/model rate lookup exists; it does not establish a bill. D7 sender/room rate limits and queue bounds constrain invocation pressure. Cross-organization `maxTokens` reserves admission capacity per verified client; actual model telemetry remains aggregate. See [Observability §9](observability.md) for metric labels, dashboard panels, privacy limits, and the token-cost boundary, and the [agentgateway observability reference](https://agentgateway.dev/docs/standalone/latest/reference/observability/) for the upstream telemetry surface.

## 3. Scale without changing semantics

Only `clusters/gcp` composes [`infra/production-ha/cluster`](../infra/production-ha/cluster). The `local`, `demo`, and `federation` profiles intentionally do not. Use the component table and evidence limits in [Production Installation: Availability posture](production.md#availability-posture) before changing capacity.

1. Measure normal traffic, a representative rollout, and the intended failure condition. Retain headroom for scheduling and recovery.
1. Change workload values or the three namespace quota profiles in `clusters/<env>/platform-settings.yaml`; do not patch generated Deployments or individual ResourceQuota objects.
1. Render and validate the effective overlay. A ResourceQuota is a ceiling, not a reservation or measured target.
1. Reconcile through Flux and verify placement, readiness, disruption behavior, and queue/latency signals on the target cluster.
1. Record the observed scope. The default two-node, one-zone GKE reference demonstrates host separation, not zone or region availability.

The Matrix-to-A2A bridge is intentionally one ready intake replica. Its chart rejects `replicaCount` values other than `1` to preserve per-room ordering. On graceful termination it stops intake and claims, closes transaction connections, and drains active leases for up to 25 seconds within a 45-second Pod grace period; nonterminal canceled work stays recoverable. `mise run test:availability` proves that graceful SIGTERM path and deduplication with a replacement process. It does not prove SIGKILL, node loss, automatic Synapse retry, or a production RTO.

`mise run test:crash-recovery` separately exercises six persisted SIGKILL boundaries. If A2A transport may have started but returned no acknowledgement, recovery records `ambiguous` and does not resend: deterministic message IDs do not establish target idempotency. This is at-most-once recovery at the uncertain boundary, not distributed exactly-once delivery. Run target-specific drills before setting an SLO or RTO.

## 4. Back up and restore the state and its keys

### 4.1 Current declared posture

| Scope               | Checked-in posture                                                                                                                                                                        | Boundary                                                                                                                                                                                                       |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| GCP `platform-pg`   | CloudNativePG native `barmanObjectStore` to GCS through Workload Identity; continuous WAL, 30-day retention, and `platform-pg-nightly` at 03:00 daily with an immediate first base backup | This is the repository's current implementation. Upstream has deprecated native Barman Cloud configuration in favor of its plugin; [#170](https://github.com/fmind-ai/fgentic/issues/170) owns that migration. |
| Local `platform-pg` | The overlay removes the GCS backup block, service-account template, and ScheduledBackup                                                                                                   | Local development does not inherit a production backup merely because the base contains one.                                                                                                                   |
| GCP Synapse media   | ESS retains `ess-synapse-media` on `standard-rwo`; `fgentic-synapse-media` retains PD CSI snapshot contents                                                                               | No snapshot is created merely because the class exists. [#64](https://github.com/fmind-ai/fgentic/issues/64) owns the coordinated database+media restore evidence.                                             |
| Local Synapse media | ESS retains `ess-synapse-media` on `local-path`                                                                                                                                           | k3d local-path has no snapshot boundary. This is development persistence, not a backup or production recovery claim.                                                                                           |
| SOPS resources      | Ciphertext is committed under `clusters/<env>/secrets/*.sops.yaml`; the matching age private key remains outside Git and is installed as cluster-local `sops-age` material                | A database backup does not contain Kubernetes Secrets or the external age private key.                                                                                                                         |

CloudNativePG point-in-time recovery consumes a valid base backup and WAL archive into a **new** Cluster; it is not an in-place rewind. Follow the [CloudNativePG recovery reference](https://cloudnative-pg.io/documentation/current/recovery/) for the pinned operator version and review the recovery manifest before reconciliation.

### 4.2 Backup checks

1. Require the CloudNativePG Cluster and the latest Backup objects to report success, and confirm WAL archival remains current.
1. Verify retention and object-store access from the intended Workload Identity; never copy a provider key into a Pod as a shortcut.
1. On GKE, require `ess-synapse-media` to use `standard-rwo`, the PD CSI addon and snapshot CRDs to be available, and the selected media `VolumeSnapshot` to report `readyToUse: true` before treating it as a recovery point.
1. Preserve the exact SOPS ciphertext revision and test access to the original age recovery key through the organization's approved offline procedure.
1. Alert on failed or stale backup evidence. Do not infer restorability from object presence alone.

### 4.3 Isolated restore drill

1. Choose a reviewed recovery point and create a separately named CloudNativePG recovery Cluster in an isolated namespace or cluster. Do not overwrite the production Cluster.
1. Upload a deterministic media fixture and record its content hash. Quiesce Synapse media writes, create an explicitly named `VolumeSnapshot` of `ess-synapse-media` with class `fgentic-synapse-media`, wait for `readyToUse: true`, and record the database and media recovery timestamps. A retained class without a ready snapshot is not a backup.
1. Restore the exact committed SOPS ciphertext with the original age key. `scripts/gen-secrets.sh` creates new credentials; it is not recovery of the old credential set.
1. Provision a new media PVC from that exact snapshot and attach only the isolated Synapse recovery instance. Verify the fixture downloads with the original hash; do not infer media recovery from a bound PVC alone.
1. Verify every expected database, scoped role, collation requirement, extension such as pgvector, ownership grant, and HBA boundary before connecting consumers.
1. Verify Matrix, MAS, bridge durable state, Keycloak, kagent, knowledge, and every enabled optional bridge against the recovered credential pairs. Appservice, database, OIDC, A2A, and MCP copies must remain coherent.
1. Run positive and negative service checks, then record the observed recovery point, data loss, duration, revision, and deviations. Only those observations may become RPO/RTO evidence.
1. Destroy the isolated recovery environment only after its evidence and retention decision are recorded.

Use `scripts/rotate-secrets.sh` and the [SOPS secrets runbook](../.agents/skills/sops-secrets/SKILL.md) for an intentional coherent rotation after recovery. Never regenerate one half of a paired credential or place a model credential in an Agent.

## 5. Respond from the signal, not the guess

| Signal or symptom                                 | Enforcing control and file                                                                                                                                                                                                                                                                                                      | Diagnose and contain                                                                                                                                                                                                                                                                  | Do not claim or do                                                                                                                                                 |
| ------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `LLMTokenBurnHigh`                                | The threshold and recording rule live in [`cost-alert.yaml`](../infra/observability/monitors/cost-alert.yaml); bridge sender/room enforcement is in [`handler.go`](../apps/matrix-a2a-bridge/internal/bridge/handler.go) and its bounded configuration in [`values.yaml`](../apps/matrix-a2a-bridge/chart/values.yaml) (D7/D8). | Break down token growth by provider/model/route/token type and correlate bridge outcomes and queue. Stop a loop at the sender/room allowlist or Agent mapping, retain rate/queue bounds, and correct the reviewed route or budget through Git.                                        | Do not call the threshold currency spend, per-user usage, or a hard cap. Do not raise it merely to silence the alert.                                              |
| Rising queue/inflight or delegation latency       | Bridge gauges and histograms are declared in [`metrics.go`](../apps/matrix-a2a-bridge/internal/bridge/metrics.go); production scaling composition is [`infra/production-ha/`](../infra/production-ha/).                                                                                                                         | Check downstream A2A, agentgateway/model, Postgres, and Matrix before capacity. Separate `SendMessage` latency from polling; remove the bottleneck or reduce admitted pressure, then scale only a component whose protocol and measured headroom support it.                          | Do not increase every quota or bridge replicas.                                                                                                                    |
| Durable `ambiguous`, `dead`, or recovery failures | The durable state machine and fail-closed uncertain boundary are in [`durable_job.go`](../apps/matrix-a2a-bridge/internal/bridge/durable_job.go); transitions and recovery results are exported by [`metrics.go`](../apps/matrix-a2a-bridge/internal/bridge/metrics.go).                                                        | Inspect the affected content-free job identity and persisted transition. Preserve evidence, resolve downstream state explicitly, and use a new user-authorized request if another action is required.                                                                                 | Never resend `ambiguous` work automatically or relabel it success to clear a dashboard.                                                                            |
| Suspected prompt injection or tool abuse          | Per-Agent full-MXID `allowedSenders`, `allowedServers`, and MCP policy are parsed in [`config.go`](../apps/matrix-a2a-bridge/internal/config/config.go); [Security §7](security.md) maps the room, NetworkPolicy, gateway, and tool enforcement points.                                                                         | Treat Matrix content and external artifacts as untrusted. Review room membership, sender/server rules, exact Agent mapping, MCP subset, downstream identity, and egress. Disable only the compromised route, rotate exposed credentials coherently, and retain content-free evidence. | Do not paste room content, prompts, credentials, or private artifacts into public issues or support bundles.                                                       |
| Federated peer unexpectedly allowed or denied     | Closed homeserver allowlists are in [`infra/federation/matrix-a/`](../infra/federation/matrix-a/); the room-v12 `m.room.server_acl` constructor is [`federation-matrix.sh`](../scripts/lib/federation-matrix.sh), and callback policy reload is owned by [`federation.sh`](../scripts/federation.sh) (D6).                      | Check the ACL, closed-federation allowlist, callback policy version, Signed AgentCard pin, JWT client, and exact route independently. Revoke or restore the narrow plane through Git/Flux and use [Federation Offboarding](federation-offboarding.md) when trust is withdrawn.        | `fed:policy-reload` is an owned disposable-lab proof that restores deny; it is not a production mutation command. Reachability alone is not governance acceptance. |
| Flux object not Ready                             | The ordered production DAG is [`clusters/base/`](../clusters/base/); Flux Kustomization dependencies and health checks are the reconciliation enforcement point.                                                                                                                                                                | Find the earliest failed source/Kustomization/HelmRelease and inspect its conditions/events. Correct the source revision, dependency, substitution, secret, or health check in Git, then reconcile and observe the DAG.                                                               | Do not bypass Flux with a manual production apply or upgrade.                                                                                                      |
| Backup failed or restore point is stale           | GCS/WAL retention is in [`cluster.yaml`](../infra/postgres/cluster.yaml), and the base-backup schedule is [`scheduledbackup.yaml`](../infra/postgres/scheduledbackup.yaml) (D12).                                                                                                                                               | Inspect Backup and WAL archival state plus object-store identity and retention. Restore archival, take a valid backup, and run the isolated recovery drill before revising recovery claims.                                                                                           | Do not delete the last valid chain or claim a backup success proves restore.                                                                                       |
| `TrivyImageVulnerabilityDrift`                    | The digest-scoped recording and alert rules are in [`trivy-alert.yaml`](../infra/observability/monitors/trivy-alert.yaml); the reconciled scanner boundary is [`infra/trivy-operator/`](../infra/trivy-operator/).                                                                                                              | Inspect report age, affected digest/workload, severity, fix availability, and signed source image. Upgrade the reviewed pin through the supply-chain gates or document the owned exception.                                                                                           | The alert measures report-count drift after its window; it is not proof of exploitability or complete risk.                                                        |

Security vulnerabilities use the private path in [SECURITY.md](../SECURITY.md), never a public issue. For component failures, preserve the minimum content-free reproduction and route it according to the ownership matrix below.

## 6. Upgrade and roll back through Git

1. Read the owning topic spec, [stability contract](stability.md), release notes, and current upstream compatibility guidance. Stable public-surface breaks require the documented deprecation/upgrade path; settled design changes require an ADR.
1. Confirm a valid recovery point, SOPS-key recovery, scheduling headroom, and an explicit rollback revision before changing pins.
1. Change the canonical chart/image/manifests in one reviewable Git change. Run focused render/schema tests, then the repository `check` and `test` gates sequentially.
1. Reconcile through Flux and inspect the first non-Ready layer. Run component smoke, negative-control, data, metric, and representative rollout checks on the target cluster.
1. Roll back by reverting the reviewed Git change and reconciling. Do not hand-edit a HelmRelease, Deployment, or digest to create an unrecorded state.

The following pins are coupled operational contracts, not independent search-and-replace values:

- Gateway API v1.5.1 **experimental**, Traefik chart 41.0.2 (proxy v3.7.6), and agentgateway v1.3.1 are the current supported overlap: Traefik and agentgateway both support Gateway API 1.5, while agentgateway 1.3 still watches TCPRoute v1alpha2. Gateway API v1.6 deprecates rather than removes that API version, but remains outside agentgateway 1.3.x's supported range; move the coupled pins together.
- kagent charts come from a `HelmRepository` with `type: oci`; do not replace it with an `OCIRepository`, because Flux digest build metadata then enters labels generated by kagent.
- Bridge CD builds, scans, signs, and commits the immutable multi-architecture image digest into its HelmRelease. Do not edit that digest by hand or deploy `latest`.
- After recreating the full local cluster, run `mise run cluster:overrides` to restore the gitignored `platform-settings-overrides` ConfigMap when present.

Use the [kagent debug](https://kagent.dev/docs/kagent/operations/debug) and [operational considerations](https://kagent.dev/docs/kagent/operations/operational-considerations) references for kagent-owned behavior; Fgentic's rendered compatibility and negative-control gates remain required around it.

## 7. Own the composition; escalate component internals

| Surface                         | Fgentic/operator ownership                                                                                                     | Component escalation boundary                                                                     |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------- |
| GitOps and environment contract | Overlay composition, substitutions, DAG dependencies, reviewed revision, SOPS integration, and acceptance evidence             | Flux source/reconcile/controller internals after the composition and object evidence are isolated |
| Matrix collaboration            | Room and appservice policy, homeserver-neutral bridge contract, identity mapping, federation controls, and deployment evidence | ESS/Synapse/MAS/Element internals with the exact pinned version and sanitized reproduction        |
| Delegation                      | Bridge lifecycle, durable ledger, allowlists, A2A route policy, remote-card pin, and fail-closed results                       | kagent or A2A SDK/runtime internals after the bridge boundary is isolated                         |
| Model path                      | agentgateway route, credential boundary, NetworkPolicy, provider inventory, token telemetry, and budget warning                | agentgateway or selected model-provider behavior after route-level evidence is isolated           |
| Data and recovery               | CNPG composition, scoped roles/databases, recovery manifests, SOPS pairing, restore drills, and recorded RPO/RTO               | CloudNativePG/PostgreSQL internals with backup/WAL/operator evidence and no secrets               |
| Security and supply chain       | Admission policy, least privilege, signed/digest-pinned artifacts, vulnerability response, and private disclosure              | Upstream vulnerability or component defect after Fgentic containment remains in force             |

The operator remains accountable for the assembled service even when an upstream component owns the defect. Escalation never means disabling Fgentic's negative controls, publishing secrets/content, or claiming upstream acceptance before it occurs.

## Publication status

This handbook is repository source, not proof of publication or a completed production drill. Delivery through the planned documentation site remains tracked by [#72](https://github.com/fmind-ai/fgentic/issues/72). The M31 [tenancy epic #384](https://github.com/fmind-ai/fgentic/issues/384) checklist update remains a separate lane-owned repository action; this source change does not claim it.
