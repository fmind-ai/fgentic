---
name: terraform-gke
description: Provision and manage the GKE reference cluster with Terraform — bootstrap state bucket first, plan/apply gated on maintainer approval (spend), outputs and DNS wiring, and cost levers. Use for any change under infra/terraform/ or GKE cluster operations.
metadata:
  author: Médéric Hurier (Fmind)
  created: 2026-07-11
---

# Terraform / GKE Reference

`infra/terraform/` provisions the cloud reference (your GCP project, set in `terraform.tfvars`): VPC, private-node GKE + Cloud NAT, Workload Identity, CNPG backups bucket, optional DNS, enabled APIs. Workloads stay provider-independent — Terraform stops at the cluster; everything above it is Flux (`clusters/gcp`).

## Spend gate (hard rule)

1. Cluster creation/scale-up is **gated on explicit maintainer approval** (issue #59; `needs-human`). Current state: bootstrap + APIs applied, **no cluster**. `terraform plan` and validation are always fine; **never `terraform apply` or `destroy` anything billable without the approval in hand.** Prepare the change, then hand off.

## Workflow

1. **One-time bootstrap** (run once per project): `terraform -chdir=infra/terraform/bootstrap init && … apply` with **local** state — it creates the versioned GCS tfstate bucket the main module's partial `backend "gcs"` points at (chicken-and-egg: the bootstrap state never moves remote). Then `terraform -chdir=infra/terraform init -backend-config="bucket=<state_bucket_name>"` (the bucket from bootstrap's `state_bucket` output).
1. Variables: `cp terraform.tfvars.example terraform.tfvars` (git-ignored; only the example is committed). Defaults: `europe-west1`, `e2-standard-4` × 2 nodes, zonal, `deletion_protection = false`.
1. Change loop: edit → `mise run format` (terraform fmt) → `mise run check:terraform` (fmt-check + `validate` run backend-less in CI — no creds needed) → `terraform plan` → PR → apply only with approval. Installed commit/push hooks serialize the complete gates across worktrees; in a hookless environment, run `mise run agent:gate` once near PR readiness.
1. After apply: `terraform output -raw gke_connect_command` to get credentials; point DNS A records (`fgentic.fmind.ai` + `chat.`/`matrix.`/`auth.`/`id.`/`grafana.`) at `terraform output -raw ingress_ip`; then Flux bootstrap with `--path=clusters/gcp` (matrix-agents runbook).

## Boundaries & conventions

1. Terraform manages **cloud primitives only** — never Kubernetes workloads, Helm releases, or anything Flux owns. If it runs _in_ the cluster, it belongs in `infra/<layer>/` + `clusters/`.
1. GKE-specific behavior that local k3d cannot exercise: Dataplane V2 **enforces** the NetworkPolicies (load-bearing security controls), Workload Identity provides agentgateway's Vertex AI auth (`auth.gcp {}` — no ADC secret), CNPG WAL-archives to the backups bucket.
1. Cost levers: fewer/smaller nodes and single replicas to trim; `node_count`/`machine_type` up and CNPG `instances: 3` to scale. `terraform destroy` (approval required) tears the reference down cleanly — state and DNS survive in the bootstrap bucket/registrar.
1. Provider versions are pinned (`~>` constraints + committed `.terraform.lock.hcl`) — bump via the upgrade-tools flow, not ad hoc.
