---
type: Runbook
title: Forking & Adapting
description: The checklist to run Fgentic under your own GitHub org, domain, GCP project, and container registry.
---

# Forking & Adapting Fgentic

Fgentic's workloads are plain Kubernetes and run on any conformant cluster; only the GKE reference under `infra/terraform/` is cloud-specific. Most deployment identity is already parameterized through the per-cluster `platform-settings` ConfigMap (`clusters/<env>/platform-settings.yaml`), injected everywhere by Flux `postBuild` substitution. A few identifiers live **outside** substitution (image references, the Terraform backend, the age recipient) and must be changed by hand. This page is the exhaustive list.

> Tip: run `git grep -n fmind-ai` and `git grep -n fgentic.fmind.ai` after adapting to confirm no reference to the upstream deployment remains.

## 1. Parameterized — edit one place per cluster

Set these in `clusters/<env>/platform-settings.yaml` (or, for values you want to keep out of git, an untracked `platform-settings-overrides` ConfigMap — see §5):

| Key                                                      | What it is                                                                                                                                                                  |
| -------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `server_name`                                            | Matrix `server_name` / apex domain; every `chat.`/`matrix.`/`auth.`/`id.`/`grafana.` host derives from it.                                                                  |
| `acme_email`                                             | Let's Encrypt account for cert-manager (unused locally).                                                                                                                    |
| `gcp_project`                                            | GCP project for the Vertex model backend + CNPG backup service account (needed even locally with the `vertex` profile — or switch `llm_provider` to `vllm`/an API profile). |
| `pg_backups_bucket`                                      | Globally-unique GCS bucket for CloudNativePG WAL archiving (GKE only).                                                                                                      |
| `cluster_issuer`, `llm_provider`, `llm_model`, quotas, … | Model backend and sizing — see [models.md](models.md).                                                                                                                      |

## 2. Container registry & supply chain — hardcoded `fmind-ai`, change for a fork

The CD workflow **builds** to `ghcr.io/${{ github.repository_owner }}/…`, so a fork auto-publishes to its own org. But the **deploy + verify** side pins the upstream owner and is **not** rewritten by CD's digest-pin step — a fork that skips this keeps pulling upstream images/charts and fails signature verification. Change the owner (`fmind-ai` → your org) in:

- `apps/matrix-a2a-bridge/deploy/helmrelease.yaml` — `OCIRepository.url`, `image.repository`, and the cosign `verify.matchOIDCIdentity` subject.
- `apps/matrix-a2a-bridge/chart/values.yaml` — `image.repository`.
- `apps/activitypub-agent-gateway/deploy/helmrelease.yaml` + `chart/values.yaml` — same, for the AP gateway image.
- `.github/workflows/cd.yml` — the cosign **verify** identity regex (`fmind-ai/fgentic`).
- `scripts/check-supply-chain.sh` — the expected chart URL and signing subject.
- `apps/*/chart/Chart.yaml`, `infra/bridges/chart/Chart.yaml` — `home`/`sources` links.

The GHCR packages must be **public** before Flux's keyless OCI verification can pull them (CD enforces this on `main`).

## 3. Secrets — swap the age recipient, regenerate everything

- `.sops.yaml` — replace the `age1…` **public** recipient with your own (`age-keygen`). Keep your private key out of the repo (`~/.config/sops/age/keys.txt`; mirror it into CI as `SOPS_AGE_KEY`).
- Regenerate every per-cluster secret against your recipient: `scripts/gen-secrets.sh <server_name> <env>` → `clusters/<env>/secrets/*.sops.yaml` (committed, encrypted). See the `sops-secrets` skill.

## 4. GKE reference (optional — `infra/terraform/`)

Only if you deploy the cloud reference:

- `infra/terraform/terraform.tfvars` (gitignored; copy from `terraform.tfvars.example`) — `project_id`, your `/32` in `master_authorized_networks`, optional `domain`.
- `infra/terraform/providers.tf` — `backend "gcs".bucket` (backend blocks can't use variables; set it here or pass `terraform init -backend-config="bucket=<project>-tfstate"`).
- Bucket defaults are globally unique, so override for a fork: `state_bucket_name` (`infra/terraform/bootstrap/main.tf`) and `pg_backups_bucket_name` (`infra/terraform/variables.tf`).
- Run the one-time bootstrap first (`terraform -chdir=infra/terraform/bootstrap apply`), then `terraform -chdir=infra/terraform init -migrate-state`. See the `terraform-gke` skill.

## 5. Keeping a private value out of git (local override)

To keep your real GCP project (or any value) untracked while committing a portable placeholder, use the optional `platform-settings-overrides` ConfigMap — Flux lists it after `platform-settings` in every Kustomization's `substituteFrom`, so its keys win at reconcile time:

```bash
cp clusters/local/platform-settings-overrides.example.yaml \
   clusters/local/platform-settings-overrides.yaml     # the copy is gitignored
$EDITOR clusters/local/platform-settings-overrides.yaml  # set gcp_project, etc.
mise run cluster:overrides   # applies the ConfigMap + reconciles (idempotent)
```

`mise run cluster:overrides` wraps the `kubectl apply` + `flux reconcile kustomization flux-system --with-source` (the platform is split into many Kustomizations rooted at `flux-system`; consumers pick up the value on their next reconcile) and is safe to re-run — **do it after every cluster recreate**, since the override is untracked and a recreate loses it (Vertex then falls back to the `your-gcp-project` placeholder). The committed `platform-settings.yaml` keeps that placeholder so nothing private lands in git.

## 6. Flux Git source

`clusters/<env>/flux-system/gotk-sync.yaml` points Flux at this repository. Re-run `flux bootstrap` against your fork (or edit the `GitRepository` URL) so Flux reconciles from your Git remote. Flux is provider-neutral — adapt the source for GitHub, GitLab, or a self-hosted Git host. See [production.md](production.md).
