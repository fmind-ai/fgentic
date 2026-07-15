---
type: Reference
title: Adopter Decision Brief
description: Vendor-neutral decision and parameterized TCO model for sovereign self-hosting versus a tenant-anchored managed agent platform.
---

# Adopter decision brief

## 1. Executive decision

Fgentic is a fit when an organization needs humans and agents to collaborate in operator-controlled Matrix rooms, must choose where model prompts travel, or needs a standards-based path for collaboration across sovereign organizations. It is not a managed agent suite. The project owns the Matrix-to-A2A bridge, the governance and federation composition, and the reference deployment; it composes the agent runtime, gateway, data store, model serving, and collaboration stack from independently operated open-source projects.

The choice is therefore not “free software versus a subscription.” It is a boundary decision:

| Procurement question    | Choose a sovereign Fgentic deployment when…                                                                                                     | Prefer a tenant-anchored managed platform when…                                                          | Evidence to obtain before approval                                                                       |
| ----------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| Data boundary           | The organization must operate the collaboration plane and select an in-cluster model path, or must explicitly approve each external model path. | The provider's documented processing locations, retention, subprocessors, and contract meet policy.      | Approved data-flow diagram and model profile; provider contract where an API profile is selected.        |
| Egress control          | Platform operators can own Kubernetes NetworkPolicy, agentgateway routing, credentials, and negative tests.                                     | The organization accepts the provider's policy and evidence surface instead of operating those controls. | Positive and negative egress tests; named control owner.                                                 |
| Cross-organization work | Independent organizations need Matrix-room collaboration plus explicitly authorized A2A delegation without sharing one platform tenant.         | Guest access or provider-defined external collaboration is sufficient.                                   | Federation agreement, ACLs, Signed AgentCard and transport-authorization evidence.                       |
| Operating model         | The organization can staff Kubernetes, Matrix, PostgreSQL, identity, observability, backup, security patching, and incident response.           | It deliberately transfers most runtime operations to a contracted provider.                              | Named service owner, on-call coverage, recovery objectives, and support route.                           |
| Exit                    | Protocol seams and a tested, layer-specific migration plan are worth the operating cost.                                                        | Contractual export and termination provisions are sufficient.                                            | Completed [exit evidence](exit-strategy.md#minimum-exit-evidence), or the managed contract's equivalent. |

Do not approve either option from a single headline price. Fill in the same period, workload, labor, resilience, and exit assumptions in §4–§5. A lower modeled total is meaningful only after the non-cost gates in §7 pass.

## 2. Product boundary: owned versus composed

The [architecture non-goals](architecture.md#1-vision--goals) are the pricing boundary. Fgentic does not reimplement an agent framework, gateway, Matrix distribution, database operator, or model server.

| Surface                               | Relationship                                                                                                                                       | Repository evidence                                                                                                                                                                                                              | Cost owner in the model                                                      |
| ------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------- |
| Matrix-to-A2A bridge                  | **Owned:** delegation intake, routing allowlist, durable bridge state, attribution, rate/capacity limits, and Matrix reply semantics.              | [`apps/matrix-a2a-bridge/`](../apps/matrix-a2a-bridge/), [Bridge Specification](bridge.md)                                                                                                                                       | Bridge compute, database share, releases, and bridge operations.             |
| Governance and federation composition | **Owned:** reviewed manifests, policy wiring, federation profile, runbooks, and acceptance contracts.                                              | [`clusters/`](../clusters/), [`infra/federation/`](../infra/federation/), [Federation Spec](federation.md)                                                                                                                       | Policy review, reconciliation, partner onboarding, and evidence work.        |
| Agent runtime                         | **Composed:** kagent owns Agent execution and its A2A server.                                                                                      | [`infra/kagent/`](../infra/kagent/), [Architecture §2](architecture.md#2-architecture-current-verified-state)                                                                                                                    | Runtime compute, upgrades, support, and agent authoring.                     |
| Routing, quotas, and model egress     | **Composed:** agentgateway owns proxying; Fgentic selects and constrains it. The model credential terminates there, not in an Agent or the bridge. | [`infra/agentgateway/`](../infra/agentgateway/), [D7](design-decisions.md#d7--rate-limits-as-llm-spend-guards-was-none), [D16](design-decisions.md#d16--sovereign-model-profiles-decided-2026-07-11-implementation-milestone-m1) | Gateway compute, provider usage, policy maintenance, and observability.      |
| Collaboration and identity            | **Composed:** Matrix, Synapse, MAS, Element, and the selected IdP own their product internals.                                                     | [`infra/matrix/`](../infra/matrix/), [`infra/keycloak/`](../infra/keycloak/), [Licensing §10.2](licensing.md#102-the-honest-agplopen-core-map-the-no-strings-attached-audit)                                                     | Homeserver, client, identity, support, and license-review costs.             |
| Durable storage                       | **Composed:** CloudNativePG operates PostgreSQL; Fgentic declares service-scoped databases, backup intent, and access boundaries.                  | [`infra/postgres/`](../infra/postgres/), [D12](design-decisions.md#d12--data-durability-was-zero-backups)                                                                                                                        | Database compute/storage, object storage, restore exercises, and DBA effort. |
| Model serving                         | **Composed:** vLLM or an API provider performs inference; Fgentic selects one profile per cluster.                                                 | [`infra/models/`](../infra/models/), [Model Provider Profiles](models.md)                                                                                                                                                        | Self-hosted serving capacity or reviewed API token rates.                    |
| Cluster and delivery                  | **Composed:** Kubernetes and Flux operate the runtime and reconciliation APIs; the GKE module is one optional reference.                           | [`infra/terraform/`](../infra/terraform/), [`clusters/base/`](../clusters/base/)                                                                                                                                                 | Control plane, nodes, disks, network, GitOps, and platform engineering.      |

This allocation prevents double counting. For example, model-serving capacity belongs to the model line, not to the bridge; PostgreSQL backup belongs to the storage line, not to every service independently.

## 3. Vendor-neutral head-to-head

| Dimension                 | Sovereign Fgentic deployment                                                                                                                                                         | Proprietary or tenant-anchored managed platform                                                                                                      | Fgentic evidence and limitation                                                                                                                                                                                                      |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Sovereignty and residency | The operator controls the cluster and can select self-hosted vLLM. Selecting any API profile still sends complete model requests and responses outside the cluster.                  | Processing location, retention, and subprocessors are provider and contract properties.                                                              | [Model data-flow map](models.md#data-flow-and-residency). A profile endpoint does not prove account-level residency or retention.                                                                                                    |
| Model egress              | Local model traffic crosses agentgateway, which is the credential and telemetry chokepoint; reviewed NetworkPolicies constrain the path.                                             | Egress and credentials are governed through the provider's controls.                                                                                 | [`infra/agentgateway/`](../infra/agentgateway/) and [Production model boundary](production.md#choose-the-model-boundary). This is not a claim of universal DLP.                                                                      |
| Invocation cost safety    | Bridge sender/room token buckets, bounded queues, gateway token metrics, and alerts limit or expose spend paths. Cross-org `maxTokens` is an admission reservation, not consumption. | Quotas, budgets, and metering depend on the contracted product.                                                                                      | [D7](design-decisions.md#d7--rate-limits-as-llm-spend-guards-was-none), [D8](design-decisions.md#d8--loop-safe-reply-semantics-was-loopable), and [Observability](observability.md). Limits reduce risk; they do not predict a bill. |
| Lock-in and exit          | Matrix, A2A, MCP, Kubernetes, PostgreSQL, Gateway API, OIDC, and Git provide replacement seams. State and implementation behavior are not automatically portable.                    | Export formats, API continuity, identity, and termination assistance are contractual.                                                                | [Exit Strategy](exit-strategy.md) names per-layer data, recreation, cost, and one-way doors.                                                                                                                                         |
| Cross-org reach           | Matrix federation supplies the shared-room plane; Signed AgentCards plus reviewed JWT or mTLS authorization supply the direct delegation boundary.                                   | External collaboration follows the provider's tenant, guest, federation, or integration model.                                                       | [Federation Spec](federation.md). The repository proves its provider-free lab, not interoperability with every organization.                                                                                                         |
| Customization             | Source, manifests, policy, and deployment topology are reviewable and changeable by the operator.                                                                                    | Extension points and release cadence are provider-defined.                                                                                           | Custom code increases testing, upgrade, security, and support ownership; it is not free flexibility.                                                                                                                                 |
| Assurance                 | The operator can inspect code and run positive/negative controls, but must produce and retain its own operational evidence.                                                          | The buyer evaluates the provider's attestations, reports, contractual controls, and incident process.                                                | A green static render is not production evidence; [Production gates](production.md#production-gates) remain deployment-specific.                                                                                                     |
| Operations                | The adopter owns upgrades, vulnerabilities, capacity, backup/restore, identity, federation policy, and incidents, with upstream projects as escalation targets.                      | The provider owns the contracted service boundary; the buyer still owns configuration, data governance, identity integration, and vendor management. | [Production Installation](production.md) and §2 identify the actual ownership split.                                                                                                                                                 |
| Licensing and support     | Fgentic code is Apache-2.0; composed components retain their own licenses and support models.                                                                                        | Subscription and usage rights follow the provider contract.                                                                                          | [Licensing & Foundation Strategy](licensing.md) records the current AGPL, open-core, and permissive boundaries. Legal approval is still organization-specific.                                                                       |

## 4. Parameterized TCO input sheet

Use one currency, one tax treatment, and one comparison period for both options. Record an evidence identifier beside every value: a repository path or observed measurement for deployment quantities, and a dated quote, contract, invoice, or public rate card archived by procurement for commercial rates. Blank values mean “not evaluated,” never zero.

### 4.1 Workload and period

| Symbol   | Input and unit                              | Why it exists                                                    | Evidence source                                                                     |
| -------- | ------------------------------------------- | ---------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| `M`      | Comparison period, months                   | Normalizes recurring and annual costs.                           | Procurement decision record.                                                        |
| `U`      | Enabled users                               | Exposes seat-based managed pricing and the supported population. | Identity inventory.                                                                 |
| `A`      | Active-user fraction, `0..1`                | Avoids treating every enabled user as a model caller.            | Measured pilot usage; otherwise a documented assumption.                            |
| `D`      | Delegations per active user per month       | Converts users into agent invocations.                           | Bridge delegation metrics from [§9](observability.md), or a named pilot assumption. |
| `I`, `O` | Mean input and output tokens per delegation | Converts invocations into model volume.                          | Agentgateway token metrics; never cross-org reservations.                           |
| `H`      | Runtime hours in the period                 | Prices continuously running or scheduled infrastructure.         | Approved availability schedule.                                                     |
| `E`      | Chargeable outbound data, GiB               | Captures provider/cloud network charges when applicable.         | Cloud billing export or measured pilot traffic.                                     |

Derived token volumes are:

```text
delegations = U × A × D × M
input_million_tokens = delegations × I / 1,000,000
output_million_tokens = delegations × O / 1,000,000
```

### 4.2 Infrastructure, labor, and commercial rates

| Symbol                              | Input and unit                                                              | Repository quantity to inspect                                                                                                                                                                       | Rate evidence                                                                    |
| ----------------------------------- | --------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------- |
| `N[k]`, `H[k]`, `R_compute[k]`      | Resource count, billed hours, currency/hour for each node or service class  | GKE defaults in [`infra/terraform/variables.tf`](../infra/terraform/variables.tf); effective requests and quotas in [Production §Bound namespace compute](production.md#bound-namespace-compute)     | Dated cloud quote/rate export or internal charge rate.                           |
| `S[p]`, `R_storage[p]`              | Average provisioned GiB and currency/GiB-month by storage class             | CNPG starts at 10 GiB in [`infra/postgres/cluster.yaml`](../infra/postgres/cluster.yaml); vLLM cache requests 3 GiB in [`infra/models/vllm/model-cache.yaml`](../infra/models/vllm/model-cache.yaml) | Storage-class quote or internal rate.                                            |
| `R_in`, `R_out`                     | API currency per million input/output tokens                                | Exact observed provider/model identity from the [evaluation harness](models.md#model-profile-quality-evaluation)                                                                                     | Reviewed `fgentic.eval.pricing.v1` catalog and provider invoice.                 |
| `F_ops`, `C_ops`                    | Operations FTE fraction and fully loaded annual cost                        | Responsibilities in [Production Installation](production.md) and §2                                                                                                                                  | Approved workforce model.                                                        |
| `F_sec`, `C_sec`                    | Security/compliance FTE fraction and fully loaded annual cost               | Policy, evidence, vulnerability, and federation responsibilities in §2                                                                                                                               | Approved workforce model.                                                        |
| `B`, `R_backup`                     | Average retained backup GiB and currency/GiB-month                          | 30-day WAL/base-backup intent in [`infra/postgres/cluster.yaml`](../infra/postgres/cluster.yaml)                                                                                                     | Object-storage quote or internal rate.                                           |
| `X_restore`, `L_restore`, `R_labor` | Restore exercises, labor hours/exercise, currency/hour                      | [D12](design-decisions.md#d12--data-durability-was-zero-backups) and deployment recovery objectives                                                                                                  | Exercise plan and loaded labor rate.                                             |
| `R_egress`                          | Currency/GiB                                                                | Network paths selected by the model and cloud profile                                                                                                                                                | Dated provider/cloud network rate.                                               |
| `C_support`                         | Support/subscription currency for the comparison period                     | Current component and license map in [§10.2](licensing.md#102-the-honest-agplopen-core-map-the-no-strings-attached-audit)                                                                            | Signed support quote or zero with an explicit self-support decision.             |
| `R_seat`, `C_managed_usage`         | Managed-platform currency/user/month and other metered usage                | Not a Fgentic quantity                                                                                                                                                                               | Dated managed-platform quote or invoice; identify included/excluded model usage. |
| `F_managed[r]`, `C_managed[r]`      | Managed administration/security FTE fraction and annual loaded cost by role | Not a Fgentic quantity                                                                                                                                                                               | Approved workforce model; do not assume managed means zero administration.       |
| `C_managed_network`                 | Managed-platform network currency for the period                            | Not a Fgentic quantity                                                                                                                                                                               | Dated quote, invoice, or explicit included-service statement.                    |
| `C_managed_backup_export`           | Managed backup/export currency for the period                               | Outcome compared with [Exit Strategy](exit-strategy.md)                                                                                                                                              | Dated quote and the exact restore/export outcome it covers.                      |
| `C_managed_support`                 | Managed support currency for the period                                     | Not a Fgentic quantity                                                                                                                                                                               | Dated support quote or explicit included-service statement.                      |
| `C_integration[o]`, `C_exit[o]`     | One-time integration/migration and tested-exit cost per option              | Required Fgentic boundaries from [Exit Strategy](exit-strategy.md); managed scope comes from its contract                                                                                            | Approved project estimate with scope and evidence identifier.                    |
| `K[o]`                              | Contingency fraction per option, `0..1`                                     | Applied visibly after each subtotal                                                                                                                                                                  | Organization policy; no project default.                                         |

Kubernetes requests, limits, and ResourceQuotas are capacity controls—not bills and not measured utilization. Price the selected nodes or service classes, then validate that effective workload requests and rollout headroom fit. Do not add namespace quota ceilings as a second compute charge.

## 5. TCO formulas and comparison table

### 5.1 Sovereign deployment

```text
base_compute = Σk (N[k] × H[k] × R_compute[k])
platform_storage = Σp (S[p] × M × R_storage[p])
api_model = input_million_tokens × R_in + output_million_tokens × R_out
self_hosted_model = Σm (N[m] × H[m] × R_compute[m])
operations = (F_ops × C_ops + F_sec × C_sec) × M / 12
backup_and_restore = B × M × R_backup + X_restore × L_restore × R_labor
network = E × R_egress
sovereign_subtotal = base_compute + platform_storage + selected_model_path
                     + operations + backup_and_restore + network + C_support
sovereign_total = sovereign_subtotal × (1 + K[self])
                  + C_integration[self] + C_exit[self]
```

`selected_model_path` is either `api_model` or `self_hosted_model` for a mutually exclusive profile. Model PVCs are already part of `platform_storage`. If self-hosted serving uses capacity already included in `base_compute`, enter only its incremental node/service capacity in `self_hosted_model`; do not charge the same node or storage twice.

### 5.2 Tenant-anchored managed platform

```text
managed_subscription = U × M × R_seat + C_managed_usage
managed_administration = Σr (F_managed[r] × C_managed[r] × M / 12)
managed_subtotal = managed_subscription + managed_administration
                   + C_managed_network + C_managed_backup_export
                   + C_managed_support
managed_total = managed_subtotal × (1 + K[managed])
                + C_integration[managed] + C_exit[managed]
```

Use zero only when the dated quote explicitly includes the line or the organization accepts its absence. For example, an included provider backup is not equivalent to an independently tested export/restore; document which outcome the line buys.

### 5.3 Decision worksheet

| Cost line                       | Sovereign input/result | Managed input/result | Evidence ID | Included scope and exclusions |
| ------------------------------- | ---------------------: | -------------------: | ----------- | ----------------------------- |
| Compute/control plane           |                        |                      |             |                               |
| Storage/databases               |                        |                      |             |                               |
| Selected model path             |                        |                      |             |                               |
| Network/egress                  |                        |                      |             |                               |
| Backup/restore or export        |                        |                      |             |                               |
| Operations/administration labor |                        |                      |             |                               |
| Security/compliance labor       |                        |                      |             |                               |
| Support/subscription            |                        |                      |             |                               |
| Integration/migration           |                        |                      |             |                               |
| Exit exercise                   |                        |                      |             |                               |
| Contingency                     |                        |                      |             |                               |
| **Total for `M` months**        |                        |                      |             |                               |

Do not publish a crossover point unless the input sheet, effective date, currency, workload, and included service levels accompany it. “Self-hosted is cheaper” and “managed eliminates operations” are both invalid without this worksheet.

## 6. Reference profiles are inputs, not forecasts

| Profile            | What the repository declares                                                                                                                                                                                                      | Valid TCO use                                                                                                                               | Invalid inference                                                                                                                                                  |
| ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `clusters/demo`    | A disposable, deterministic protocol fixture that removes SSO, observability, Trivy, and paid-model requirements.                                                                                                                 | Estimate evaluation-machine time and engineer time only.                                                                                    | Production sizing, model quality, resilience, compliance posture, or steady-state cost.                                                                            |
| `clusters/local`   | A production-shaped k3d overlay with the tracked Vertex profile and local quota ceilings.                                                                                                                                         | Inventory components and rehearse configuration before measuring a target environment.                                                      | Treat local ceilings or laptop resources as utilization, production capacity, or cloud cost.                                                                       |
| `clusters/gcp`     | An optional GKE reference. Terraform defaults to two `e2-standard-4` nodes with 50 GiB balanced disks; the tracked overlay sets higher namespace ceilings and composes the production-HA posture. Live apply remains spend-gated. | Substitute current contracted rates, selected regional/zonal topology, observed usage, storage growth, model path, and recovery objectives. | Claim a monthly amount, production fit, SLA, or cost optimization from static manifests. The Terraform header explicitly says the reference is not cost-optimized. |
| Self-hosted `vllm` | One CPU engine requests 2 CPU/4 GiB and limits 4 CPU/6 GiB, with a 3 GiB model-cache PVC and one concurrent sequence.                                                                                                             | Establish a lower-bound reference configuration to measure, then price the accepted production override.                                    | Infer production answer quality, throughput, latency, or GPU cost. The reference exists to demonstrate sovereignty.                                                |
| CNPG               | One reference instance, a 10 GiB PVC, 30-day backup retention intent; the production posture raises instances to three.                                                                                                           | Seed storage and replica inputs, then replace them with observed growth and accepted recovery topology.                                     | Treat requested storage, retention intent, or replica count as proof of a successful restore or a service objective.                                               |

The GKE `regional` variable changes `node_count` to a per-zone quantity. Model that topology explicitly; copying the zonal node count into a regional price calculation can undercount capacity.

## 7. Non-cost approval gates

Record each result as pass, fail, or accepted risk with an owner and evidence link:

1. **Data classification and model path:** every intended room classification has an approved model boundary; API-provider contracts and account controls are reviewed where selected.
1. **Authorization:** full Matrix identities, room membership, bridge allowlists, gateway policy, and federation machine identity are tested; forwarded attribution is not mistaken for authentication.
1. **Federation:** participating homeservers, room version, server ACL, policy callback, Signed AgentCard, transport authorization, quota, and offboarding decisions are bilateral and evidenced.
1. **Operations:** platform, database, identity, model, security, incident, and upstream-support owners are named with on-call expectations.
1. **Recovery:** backups are restored in isolation and the accepted data-loss/recovery objectives are supported by evidence, not by manifest intent.
1. **Exit:** state, credentials, DNS, identity, archive, cutover, rollback, and one-way doors are exercised to the level required by procurement.
1. **Licensing and support:** every composed component's license and support path is accepted; Apache-2.0 project code does not erase upstream obligations.
1. **Capacity and cost safety:** measured requests, rollout headroom, rate limits, token alerts, billing caps, and the worksheet share the same workload assumptions.

Failure of a mandatory gate is not offset by a lower TCO. Conversely, a gate that does not apply should be marked not applicable with a reason rather than assigned a fabricated zero cost.

## 8. Decision record and publication status

Attach the completed worksheet and gate record to the organization's procurement decision with:

1. repository revision and deployment overlay;
1. comparison period, currency, tax treatment, and effective date;
1. quote, invoice, rate, measurement, and assumption identifiers;
1. selected model profile and data classification;
1. target availability and recovery objectives;
1. sensitivity cases for users, delegations, tokens, compute, storage, and labor;
1. owner and expiry date for every accepted risk.

This source document is vendor-neutral and intentionally contains no current commercial prices. The repository remains the source of truth for the Fgentic quantities and controls cited above; procurement remains the source of truth for commercial rates. Publication through the planned documentation site is tracked separately by [#72](https://github.com/fmind-ai/fgentic/issues/72) and is not implied by this Markdown file.
