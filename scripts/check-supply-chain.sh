#!/usr/bin/env bash
# Validate the safe Git-chart -> signed OCI-chart transition and permanent workflow controls.
set -euo pipefail

for command in helm rg yq; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		echo "error: required command not found: ${command}" >&2
		exit 2
	fi
done

manifest="apps/matrix-a2a-bridge/deploy/helmrelease.yaml"
workflow=".github/workflows/cd.yml"

source_value() {
	yq -er "select(.kind == \"OCIRepository\") | $1" "${manifest}"
}

release_value() {
	yq -er "select(.kind == \"HelmRelease\") | $1" "${manifest}"
}

assert_source() {
	local expression="$1"
	local expected="$2"
	local actual
	actual="$(source_value "${expression}")"
	if [ "${actual}" != "${expected}" ]; then
		echo "error: OCI source ${expression} = ${actual}; expected ${expected}" >&2
		exit 1
	fi
}

assert_release() {
	local expression="$1"
	local expected="$2"
	local actual
	actual="$(release_value "${expression}")"
	if [ "${actual}" != "${expected}" ]; then
		echo "error: HelmRelease ${expression} = ${actual}; expected ${expected}" >&2
		exit 1
	fi
}

assert_workflow() {
	local expression="$1"
	local expected="$2"
	local actual
	actual="$(yq -er "${expression}" "${workflow}")"
	if [ "${actual}" != "${expected}" ]; then
		echo "error: CD workflow ${expression} = ${actual}; expected ${expected}" >&2
		exit 1
	fi
}

assert_source '.spec.url' "oci://ghcr.io/fmind-ai/charts/matrix-a2a-bridge"
assert_source '.spec.layerSelector.mediaType' "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
assert_source '.spec.layerSelector.operation' "copy"
assert_source '.spec.verify.provider' "cosign"
assert_source '.spec.verify.matchOIDCIdentity | length' "1"
assert_source '.spec.verify.matchOIDCIdentity[0].issuer' '^https://token.actions.githubusercontent.com$'
assert_source '.spec.verify.matchOIDCIdentity[0].subject' '^https://github\.com/fmind-ai/fgentic/\.github/workflows/cd\.yml@refs/heads/main$'

suspended="$(source_value '.spec.suspend | tostring')"
if [ "${suspended}" = "true" ]; then
	# Before the first publication, the OCI source must be inert and Helm must retain the local
	# chart. Any mixed state would either reconcile a phantom artifact or bypass verification.
	if source_value '.spec.ref.digest' >/dev/null 2>&1; then
		echo "error: suspended bootstrap source unexpectedly pins a digest" >&2
		exit 1
	fi
	assert_release '.spec.chart.spec.sourceRef.kind' "GitRepository"
	if release_value '.spec.chartRef' >/dev/null 2>&1; then
		echo "error: bootstrap HelmRelease references OCI before CD activation" >&2
		exit 1
	fi
	echo "Supply-chain source state: safe pre-publication bootstrap"
elif [ "${suspended}" = "false" ]; then
	digest="$(source_value '.spec.ref.digest')"
	[[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] \
		|| {
			echo "error: active OCI chart source is not digest-pinned" >&2
			exit 1
		}
	assert_release '.spec.chartRef.kind' "OCIRepository"
	assert_release '.spec.chartRef.name' "matrix-a2a-bridge-chart"
	assert_release '.spec.chartRef.namespace' "flux-system"
	if release_value '.spec.chart' >/dev/null 2>&1; then
		echo "error: active HelmRelease still has the unverified Git chart source" >&2
		exit 1
	fi
	echo "Supply-chain source state: active signed OCI chart"
else
	echo "error: OCIRepository suspend must be explicitly true or false" >&2
	exit 1
fi

assert_workflow '.permissions."id-token"' "write"
assert_workflow '.permissions.attestations' "write"
assert_workflow '.permissions."artifact-metadata"' "write"

attest_steps="$(yq -r '.jobs."bridge-image".steps[] | .uses // ""' "${workflow}" \
	| rg -c '^actions/attest@[0-9a-f]{40}$')"
[ "${attest_steps}" -ge 3 ] \
	|| {
		echo "error: CD must attest image provenance, image SBOM, and chart provenance" >&2
		exit 1
	}
yq -r '.jobs."bridge-image".steps[] | .uses // ""' "${workflow}" \
	| rg -x 'anchore/sbom-action@[0-9a-f]{40}' >/dev/null
yq -r '.jobs."release-sbom".steps[] | .uses // ""' "${workflow}" \
	| rg -x 'anchore/sbom-action@[0-9a-f]{40}' >/dev/null
rg --fixed-strings --quiet 'cosign sign --yes "${CHART_REPOSITORY}@${{ steps.chart.outputs.digest }}"' "${workflow}"
rg --fixed-strings --quiet 'make it public before Flux is switched to keyless OCI verification' "${workflow}"

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT
helm package apps/matrix-a2a-bridge/chart \
	--destination "${workdir}" \
	--version 0.1.0-sha.0123456789ab \
	--app-version sha-0123456789ab >/dev/null
test -s "${workdir}/matrix-a2a-bridge-0.1.0-sha.0123456789ab.tgz"

echo "Supply-chain contract checks passed"
