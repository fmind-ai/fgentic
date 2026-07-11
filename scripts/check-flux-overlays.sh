#!/usr/bin/env bash
# Build the deployable cluster entrypoints through Flux's offline transformation path, then validate
# every Kubernetes object with kubeconform. This catches Kustomize components/patches and strict
# post-build substitution before a commit reaches a live reconciler.
set -euo pipefail

fixture=scripts/testdata/flux-build-kustomization.yaml
for environment in federation local gcp; do
  echo "==> Flux-building clusters/${environment}"
  build_args=(
    --path "clusters/${environment}"
    --kustomization-file "${fixture}"
    --dry-run
    --in-memory-build
    --strict-substitute
  )
  if [ "${environment}" = federation ]; then
    # The lab has no SOPS dependency, so validate its entire nested Flux graph, including the
    # A/B HelmReleases and component patches, rather than stopping at the cluster entrypoint.
    build_args+=(
      --recursive
      --local-sources GitRepository/flux-system/flux-system=.
    )
  fi
  flux build kustomization cluster-overlay-validation "${build_args[@]}" \
    | kubeconform -strict -ignore-missing-schemas -summary
done
