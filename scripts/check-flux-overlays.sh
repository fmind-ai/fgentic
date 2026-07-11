#!/usr/bin/env bash
# Build both cluster entrypoints exactly through Flux's offline transformation path, then validate
# every Kubernetes object with kubeconform. This catches Kustomize components/patches and strict
# post-build substitution before a commit reaches a live reconciler.
set -euo pipefail

fixture=scripts/testdata/flux-build-kustomization.yaml
for environment in local gcp; do
  echo "==> Flux-building clusters/${environment}"
  flux build kustomization cluster-overlay-validation \
    --path "clusters/${environment}" \
    --kustomization-file "${fixture}" \
    --dry-run \
    --in-memory-build \
    --strict-substitute \
    | kubeconform -strict -ignore-missing-schemas -summary
done
