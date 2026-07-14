---
type: Runbook
title: Bridge Supply-Chain Verification
description: How to independently verify the bridge's signed multi-arch image, SBOM, and provenance chain.
---

# Bridge Supply-Chain Verification

The bridge release chain has two independently verified deployable artifacts:

```text
source revision
  ├─ Docker Buildx → multi-arch image digest
  │    ├─ Trivy HIGH/CRITICAL gate
  │    ├─ Syft SPDX SBOM → workflow/release artifact
  │    ├─ GitHub SLSA + SBOM attestations → OCI referrers
  │    └─ Cosign keyless signature
  └─ Helm package → unique SemVer chart → OCI chart digest
       ├─ GitHub provenance attestation
       └─ Cosign keyless signature → Flux OCIRepository verification
```

## Publication and bootstrap

`.github/workflows/cd.yml` publishes the image and chart only from `main` or a `v*` tag. Every main build receives a unique chart version (`<base>-sha.<12-char-commit>`), so an OCI tag is never the deployment identity. Release tags use the release SemVer. Both artifacts are signed with the workflow's short-lived GitHub OIDC identity; no signing key is stored.

There is one deliberate bootstrap interlock. Before the first signed chart exists, `apps/matrix-a2a-bridge/deploy/helmrelease.yaml` contains a suspended `OCIRepository` and the HelmRelease still reads the in-repository chart. CD publishes and verifies the chart, then requires both `matrix-a2a-bridge` and `charts/matrix-a2a-bridge` GHCR packages to be public. Only then does it atomically pin the chart digest, unsuspend the verified source, switch the HelmRelease to `chartRef`, and pin the image digest in the same GitOps commit. This avoids reconciling a phantom or private chart during bootstrap.

Making the packages public is a one-time maintainer action in GitHub Packages. Do not replace the interlock with a long-lived registry token: Flux keyless verification is designed for publicly readable, signed artifacts. The repository itself may remain private; the Cosign signatures used by Flux are still issued from the public GitHub OIDC/Fulcio path.

## Identity policy

Flux accepts the chart only when Cosign verifies both of these certificate fields:

```text
issuer:  https://token.actions.githubusercontent.com
subject: https://github.com/fmind-ai/fgentic/.github/workflows/cd.yml@refs/heads/main
```

The matcher is exact and anchored. Tag signatures are valid for release verification, but the deployed chart source accepts only a chart published by the `main` workflow identity. Removing the `verify` block or broadening its subject is a security-boundary change.

## Operator verification

Set the immutable references from the deployed manifests:

```bash
IMAGE_REPOSITORY=ghcr.io/fmind-ai/matrix-a2a-bridge
IMAGE_DIGEST=$(yq -er 'select(.kind == "HelmRelease") | .spec.values.image.tag | split("@") | .[1]' \
  apps/matrix-a2a-bridge/deploy/helmrelease.yaml)
CHART_REPOSITORY=ghcr.io/fmind-ai/charts/matrix-a2a-bridge
CHART_DIGEST=$(yq -er 'select(.kind == "OCIRepository") | .spec.ref.digest' \
  apps/matrix-a2a-bridge/deploy/helmrelease.yaml)
IDENTITY='^https://github\.com/fmind-ai/fgentic/\.github/workflows/cd\.yml@refs/(heads/main|tags/v.*)$'
ISSUER='https://token.actions.githubusercontent.com'

cosign verify --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER" \
  "${IMAGE_REPOSITORY}@${IMAGE_DIGEST}"
cosign verify --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER" \
  "${CHART_REPOSITORY}@${CHART_DIGEST}"
gh attestation verify "oci://${IMAGE_REPOSITORY}@${IMAGE_DIGEST}" --repo fmind-ai/fgentic
```

Inspect SBOM and provenance referrers without executing the image:

```bash
cosign tree "${IMAGE_REPOSITORY}@${IMAGE_DIGEST}"
gh attestation verify "oci://${IMAGE_REPOSITORY}@${IMAGE_DIGEST}" \
  --repo fmind-ai/fgentic --predicate-type https://spdx.dev/Document
```

On a reconciled cluster, require source verification before trusting Helm status:

```bash
kubectl -n flux-system wait ocirepository/matrix-a2a-bridge-chart \
  --for=condition=SourceVerified=True --timeout=2m
flux get source oci matrix-a2a-bridge-chart
flux get helmrelease matrix-a2a-bridge -n bridge
```

An unsigned or wrong-identity chart makes the OCIRepository `Ready=False` with a verification error, so helm-controller never receives it. Image signature verification remains an operator/CI gate: Kubernetes and Flux do not enforce image signatures without a separate admission policy.

## SBOM retention

Every build stores the Syft SPDX JSON as a bounded GitHub Actions artifact and attaches the same document as an OCI SBOM attestation. When a GitHub Release is published, the release job resolves the already-published immutable release image and attaches a freshly generated SPDX JSON file to that Release. The OCI referrer is the machine-consumable source; the Release asset is the human review/download surface. Neither contains runtime secrets.

Do not claim end-to-end acceptance until the bootstrap interlock has switched the HelmRelease, the `SourceVerified=True` condition is observed, and an intentionally unsigned test chart is rejected in a disposable registry path. Local `actionlint`, chart packaging, schema validation, and signature-policy checks validate the implementation but cannot mint GitHub OIDC identities.
