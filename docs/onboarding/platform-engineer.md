---
type: Guide
title: Platform Engineer Onboarding
description: GitOps, overlays, secrets, model selection, validation, and operational ownership for a Fgentic deployment.
---

# Platform engineer onboarding

## 1. Delivery model

Production is pull-based Flux GitOps. Commit reviewed configuration and SOPS ciphertext, then let Flux reconcile the dependency DAG; do not `kubectl apply` or `helm upgrade` production workloads by hand. The pinned experimental Gateway API bundle is the only documented out-of-band CRD install; bootstrap separately creates Flux and the cluster-local SOPS decryption key.

Start with [Production Installation](../production.md) and the checked-in [`clusters/base/`](../../clusters/base/) DAG. Use the smallest environment that answers the question:

| Overlay          | Purpose                                                                                    | Do not infer                                                                                   |
| ---------------- | ------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------- |
| `clusters/demo`  | Disposable, credential-free protocol evaluation with a deterministic model fixture.        | Production model quality, SSO, observability, vulnerability, resilience, or capacity evidence. |
| `clusters/local` | Production-shaped k3d development and operator rehearsal.                                  | That laptop capacity or local credentials represent production.                                |
| `clusters/gcp`   | Optional GKE reference with production-HA components and provider-specific infrastructure. | A live apply, cost, SLA, or non-GCP production profile; GKE apply remains spend-gated.         |

## 2. Configuration and secrets

1. Set domain, issuer, provider/model, alert budget, and namespace quota profiles in `clusters/<env>/platform-settings.yaml`. Use the gitignored override ConfigMap only for values intentionally kept outside Git.
1. Select one governed model profile and confirm its residency, credential, network, and acceptance requirements in [Model Provider Profiles](../models.md).
1. Generate the environment's complete encrypted set with `scripts/gen-secrets.sh <server_name> <local|gcp>`. Real secrets belong only in `clusters/<env>/secrets/*.sops.yaml`; `infra/secrets/` contains templates.
1. Commit ciphertext. Flux decrypts it with the cluster's `sops-age` Secret; never put a decrypted value in Git, a PR, a support bundle, or a shell transcript.
1. Rotate one coherent secret set with `scripts/rotate-secrets.sh`, wait for the relevant CNPG/policy resource version, and restart only the documented consumers in [the secrets runbook](../../.agents/skills/sops-secrets/SKILL.md).

Appservice, A2A, MCP, database, and OIDC credentials have paired copies and ordered reloads. Editing one Kubernetes Secret by hand creates a split-brain credential window and is not a rotation procedure.

## 3. Change and validation loop

1. Read the owning topic spec and any settled ADR before changing manifests or versions.
1. Change canonical Helm values or parameterized manifests under `infra/`/`apps/`; keep Kustomize as the thin composition/overlay layer.
1. Run the smallest focused render/schema/contract checks while iterating. Do not run shared local clusters from a second worktree or bypass the runtime lease.
1. Let the installed commit/push hooks serialize `mise run check` and `mise run test`; CI repeats those gates and asserts a clean tree.
1. Deliver through a reviewed commit and Flux. Wait for sources, namespaces/secrets, controllers, data plane, stateful services, and apps in DAG order.
1. Capture target-cluster evidence separately. Static quotas are ceilings, not reservations or measured usage; rendered replicas are not availability proof.

Version pins that bind each other move together: Gateway API v1.5.1 experimental, Traefik chart 41.0.2, and agentgateway v1.3.1 are the deliberate current compatibility set. kagent charts use an OCI-type HelmRepository. The bridge image is built, scanned, signed, and digest-pinned by CD; never edit that digest by hand.

## 4. Day-2 ownership

At minimum, assign owners for Flux reconciliation, Kubernetes capacity, ESS/Matrix, identity, CNPG and restore drills, agentgateway/model egress, Agents/MCP, observability, vulnerabilities, certificates/DNS, federation policy, secrets, and incident response. Route component-internal failures to the appropriate upstream project without dropping the Fgentic-level negative controls.

Use the [Day-2 Operations Handbook](../operations-handbook.md) to connect monitoring, scaling, recovery, alert-keyed incident response, and upgrades to those component owners. Its repository source does not replace target-cluster evidence or the separate documentation-site publication gate.

> **Own vs compose.** Fgentic owns the GitOps composition, bridge lifecycle, policy wiring, environment contract, and validation gates. Flux, Kubernetes, ESS, CloudNativePG, kagent, agentgateway, cert-manager, Traefik, and the model backend own their internal runtime behavior. The operator owns the assembled service and its evidence.
