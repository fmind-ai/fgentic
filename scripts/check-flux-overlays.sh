#!/usr/bin/env bash
# Build the deployable cluster entrypoints through Flux's offline transformation path, then validate
# every Kubernetes object with kubeconform. This catches Kustomize components/patches and strict
# post-build substitution before a commit reaches a live reconciler.
#
# It ALSO audits the effective Flux dependency DAG per profile: every `Kustomization.spec.dependsOn`
# edge must point to a Kustomization that actually exists in that profile's rendered graph. Offline
# `flux build` does not resolve dependsOn (that is a live-reconciler concern), and kubeconform only
# checks object schemas, so a dangling edge — e.g. a profile that deletes `observability` while a
# workload still depends on it — otherwise slips through and only stalls a real cluster
# (regression #605/#609). Demo is included here for the closure audit even though its object
# schemas are owned by `check:demo`.
set -euo pipefail

fixture=scripts/testdata/flux-build-kustomization.yaml

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

# Assert every dependsOn edge in a rendered profile resolves to a defined Kustomization.
assert_dependency_closure() {
	local environment="$1" render="$2"
	local defined edges missing=0

	# Names of every Kustomization defined in this profile's effective DAG.
	defined="$(yq -e '
		select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization" and .metadata.namespace == "flux-system")
		| .metadata.name
	' "${render}" | grep -vxF -- '---' | sort -u)"

	# "<kustomization> <dependsOn-target>" for every dependency edge in the profile.
	# shellcheck disable=SC2016 # `$n` is a yq binding variable, not a shell expansion.
	edges="$(yq -e '
		select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization" and .metadata.namespace == "flux-system")
		| .metadata.name as $n | (.spec.dependsOn[]?.name | $n + " " + .)
	' "${render}" 2>/dev/null | grep -vxF -- '---' || true)"

	while read -r kustomization target; do
		[ -z "${target}" ] && continue
		if ! grep -qxF -- "${target}" <<<"${defined}"; then
			echo "Error: [${environment}] Kustomization '${kustomization}' dependsOn '${target}', absent from the ${environment} DAG" >&2
			missing=1
		fi
	done <<<"${edges}"

	[ "${missing}" -eq 0 ] || exit 1
	local count
	count="$(grep -c . <<<"${defined}")"
	echo "    dependency closure OK (${count} Kustomizations)"
}

for environment in local gcp demo federation; do
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

	render="${workdir}/${environment}.yaml"
	flux build kustomization cluster-overlay-validation "${build_args[@]}" >"${render}"

	# Demo's object schemas are validated by `check:demo`; here it only joins the closure audit.
	if [ "${environment}" != demo ]; then
		kubeconform -strict -ignore-missing-schemas -summary <"${render}"
	fi

	assert_dependency_closure "${environment}" "${render}"
done
