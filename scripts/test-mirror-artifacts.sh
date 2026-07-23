#!/usr/bin/env bash
# Offline contract test for mirror-artifacts.sh. This file also serves as the mock `skopeo` and
# `helm` executables through temporary symlinks, so the fixture can never make a live registry call.
# Real `yq` parses the fixture BOM (deterministic, no network). It asserts: dry-run performs no push
# or existence probe; apply mode issues one digest-preserving copy per BOM image/taggedImage/chart
# with the correct origin-namespaced target; classic HTTP charts pull-and-push; an already-mirrored
# target is an idempotent skip; a real copy failure fails closed non-zero; every fixture artifact is
# covered (no silent under-coverage); an unresolvable entry fails closed and loudly; and output is
# content-free (no credential leak).
set -euo pipefail

readonly REGISTRY_PASSWORD_CANARY='CANARY-REGISTRY-PASSWORD-DO-NOT-LEAK'

# --- Mock dispatch: when invoked as skopeo/helm, answer from the shared state. -------------------

mock_log() {
	[ -n "${MOCK_CALL_LOG:-}" ] || return 0
	printf '%s\n' "$*" >>"${MOCK_CALL_LOG}"
}

mock_skopeo() {
	mock_log "skopeo $*"
	local sub="${1:-}" arg last=''
	for arg in "$@"; do
		last="${arg}"
	done
	case "${sub}" in
		inspect)
			# The mirrored-state file records every target skopeo/helm has pushed so a re-run reports
			# an existing artifact as present (idempotent) exactly like a real registry would.
			local target="${last#docker://}"
			if [ -n "${MOCK_STATE_FILE:-}" ] && grep -qxF "${target}" "${MOCK_STATE_FILE}" 2>/dev/null; then
				printf '{}\n'
				return 0
			fi
			return 1
			;;
		copy)
			local dst="${last#docker://}"
			case "${MOCK_FAIL:-}" in
				skopeo-copy)
					printf 'mock skopeo copy failure\n' >&2
					return 1
					;;
				*) ;;
			esac
			[ -n "${MOCK_STATE_FILE:-}" ] && printf '%s\n' "${dst}" >>"${MOCK_STATE_FILE}"
			return 0
			;;
		*)
			printf 'unexpected skopeo call: %s\n' "$*" >&2
			return 2
			;;
	esac
}

mock_helm() {
	mock_log "helm $*"
	local sub="${1:-}"
	case "${sub}" in
		pull)
			local chart="${2:-}" version='' dest='' prev=''
			local arg
			for arg in "$@"; do
				case "${prev}" in
					--version) version="${arg}" ;;
					--destination) dest="${arg}" ;;
					*) ;;
				esac
				prev="${arg}"
			done
			case "${MOCK_FAIL:-}" in
				helm-pull)
					printf 'mock helm pull failure\n' >&2
					return 1
					;;
				*) ;;
			esac
			: >"${dest}/${chart}-${version}.tgz"
			return 0
			;;
		push)
			# helm push <tgz> oci://<repo>. Record the resulting chart ref so a re-run is idempotent.
			local tgz="${2:-}" ref="${3:-}" base name version repo
			base="$(basename "${tgz}" .tgz)"
			name="${base%-*}"
			version="${base##*-}"
			repo="${ref#oci://}"
			[ -n "${MOCK_STATE_FILE:-}" ] && printf '%s\n' "${repo}/${name}:${version}" >>"${MOCK_STATE_FILE}"
			return 0
			;;
		*)
			printf 'unexpected helm call: %s\n' "$*" >&2
			return 2
			;;
	esac
}

case "${0##*/}" in
	skopeo)
		mock_skopeo "$@"
		exit "$?"
		;;
	helm)
		mock_helm "$@"
		exit "$?"
		;;
	*) ;;
esac

# --- Driver ------------------------------------------------------------------------------------

for command in yq rg; do
	command -v "${command}" >/dev/null 2>&1 || {
		printf 'error: required test command not found: %s\n' "${command}" >&2
		exit 2
	}
done

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
readonly SCRIPT="${REPO_ROOT}/scripts/mirror-artifacts.sh"
readonly TARGET='registry.internal:5000'
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-mirror-test.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

readonly BIN_DIR="${WORK_DIR}/bin"
mkdir -p "${BIN_DIR}"
ln -s "${REPO_ROOT}/scripts/test-mirror-artifacts.sh" "${BIN_DIR}/skopeo"
ln -s "${REPO_ROOT}/scripts/test-mirror-artifacts.sh" "${BIN_DIR}/helm"

# Full 64-hex digests keep the qualifier's tag/digest split realistic.
readonly D_REDIS='sha256:1111111111111111111111111111111111111111111111111111111111111111'
readonly D_KEYCLOAK='sha256:2222222222222222222222222222222222222222222222222222222222222222'
readonly D_BRIDGE='sha256:3333333333333333333333333333333333333333333333333333333333333333'
readonly D_CHART='sha256:4444444444444444444444444444444444444444444444444444444444444444'

# Fixture A: every artifact type, all fully resolvable from the BOM.
readonly BOM_OK="${WORK_DIR}/bom-ok.yaml"
cat >"${BOM_OK}" <<EOF
apiVersion: fgentic.dev/bom/v1
kind: BillOfMaterials
chartSources:
  - name: "oci-chart"
    kind: "OCIRepository"
    url: "oci://ghcr.io/example/charts/thing"
    version: "v1.2.3"
  - name: "oci-chart-digest"
    kind: "OCIRepository"
    url: "oci://ghcr.io/example/charts/pinned"
    version: "${D_CHART}"
  - name: "git-chart"
    kind: "GitRepository"
    url: "https://github.com/example/thing.git"
    version: "abc123"
helmReleases:
  - name: "rel-http"
    chart: "widget"
    version: "4.5.6"
    source: "httprepo"
  - name: "rel-oci"
    chart: "gadget"
    version: "7.8.9"
    source: "ocirepo"
  - name: "rel-chartsource"
    chart: "thing"
    version: null
    source: "oci-chart"
helmRepositories:
  - name: "httprepo"
    url: "https://charts.example.com"
  - name: "ocirepo"
    url: "oci://ghcr.io/example/helm"
images:
  - ref: "docker.io/library/redis:8.0@${D_REDIS}"
    digest: "${D_REDIS}"
    resolved: true
taggedImages:
  - registry: null
    repository: "quay.io/example/keycloak"
    tag: "26.0"
    digest: "${D_KEYCLOAK}"
    resolved: true
EOF

# Fixture B: one resolved:false image + one resolved:false tagged image are hard residuals (exit
# non-zero); the GitRepository chart source is informational (mirrored via internal Git), NOT counted.
readonly BOM_GAP="${WORK_DIR}/bom-gap.yaml"
cat >"${BOM_GAP}" <<EOF
apiVersion: fgentic.dev/bom/v1
kind: BillOfMaterials
chartSources:
  - name: "git-chart"
    kind: "GitRepository"
    url: "https://github.com/example/thing.git"
    version: "abc123"
helmReleases: []
helmRepositories: []
images:
  - ref: "0.72.0@${D_BRIDGE}"
    digest: "${D_BRIDGE}"
    resolved: false
    note: "chart-default image; repository absent from repo manifests"
taggedImages:
  - registry: null
    repository: null
    tag: "2.19.0"
    digest: "${D_KEYCLOAK}"
    resolved: false
    note: "chart-default image; repository absent from repo manifests"
EOF

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

# run <name> <apply> <bom> [state-file] [fail-mode] -> populates OUT/ERR/CALL_LOG/RC.
run() {
	local name="$1" apply="$2" bom="$3" state="${4:-}" failmode="${5:-}"
	OUT_FILE="${WORK_DIR}/${name}.out"
	ERR_FILE="${WORK_DIR}/${name}.err"
	CALL_LOG="${WORK_DIR}/${name}.calls"
	: >"${CALL_LOG}"
	set +e
	env -i \
		PATH="${BIN_DIR}:${PATH}" HOME="${WORK_DIR}" \
		MIRROR_APPLY="${apply}" MIRROR_BOM_FILE="${bom}" \
		MOCK_CALL_LOG="${CALL_LOG}" MOCK_STATE_FILE="${state}" MOCK_FAIL="${failmode}" \
		REGISTRY_PASSWORD="${REGISTRY_PASSWORD_CANARY}" \
		bash "${SCRIPT}" "${TARGET}" >"${OUT_FILE}" 2>"${ERR_FILE}"
	RC=$?
	set -e
}

# 1. Dry-run performs no existence probe and no push.
run dry no "${BOM_OK}"
[ "${RC}" -eq 0 ] || fail "dry-run exited ${RC}"
if rg --quiet '^skopeo (copy|inspect)' "${CALL_LOG}" || rg --quiet '^helm (pull|push)' "${CALL_LOG}"; then
	fail "dry-run issued a mirroring call"
fi
rg --quiet 'dry-run complete: 6 artifact\(s\) planned' "${ERR_FILE}" || fail "dry-run did not plan every artifact"
# A GitRepository chart source is informational (mirrored via internal Git), never a planned copy or a residual.
rg --quiet 'INFO: chart source git-chart: GitRepository' "${ERR_FILE}" || fail "GitRepository chart source was not reported as info"
if rg --quiet 'UNRESOLVED' "${ERR_FILE}"; then
	fail "a complete BOM reported an unresolved artifact"
fi

# 2. Every fixture artifact is covered in the plan (no silent under-coverage), digest preserved.
for expected in \
	"docker://${TARGET}/ghcr.io/example/charts/thing:v1.2.3" \
	"docker://${TARGET}/ghcr.io/example/charts/pinned@${D_CHART}" \
	"docker://${TARGET}/ghcr.io/example/helm/gadget:7.8.9" \
	"docker://${TARGET}/docker.io/library/redis@${D_REDIS}" \
	"docker://${TARGET}/quay.io/example/keycloak@${D_KEYCLOAK}" \
	"oci://${TARGET}/charts"; do
	rg --quiet --fixed-strings "${expected}" "${ERR_FILE}" || fail "plan missing target ${expected}"
done
# The redis tag must be dropped in favor of the digest (integrity, not the mutable tag).
if rg --quiet --fixed-strings "redis:8.0@${D_REDIS}" "${ERR_FILE}"; then
	fail "plan kept a redundant tag beside the digest"
fi
# A HelmRelease backed by a chartSource must not be double-mirrored as a repository chart.
rg --quiet "skip release rel-chartsource" "${ERR_FILE}" || fail "chartSource-backed release was not skipped"

# 3. Apply mode issues exactly one copy per OCI artifact plus one HTTP chart pull+push.
readonly STATE_A="${WORK_DIR}/state-a.list"
: >"${STATE_A}"
run apply yes "${BOM_OK}" "${STATE_A}"
[ "${RC}" -eq 0 ] || {
	cat "${ERR_FILE}" >&2
	fail "apply exited ${RC}"
}
rg --quiet --fixed-strings "skopeo copy docker://ghcr.io/example/charts/thing:v1.2.3 docker://${TARGET}/ghcr.io/example/charts/thing:v1.2.3" "${CALL_LOG}" \
	|| fail "apply did not copy the OCI chart with a namespaced target"
rg --quiet --fixed-strings "skopeo copy docker://docker.io/library/redis@${D_REDIS} docker://${TARGET}/docker.io/library/redis@${D_REDIS}" "${CALL_LOG}" \
	|| fail "apply did not copy the image by digest"
rg --quiet --fixed-strings "skopeo copy docker://quay.io/example/keycloak@${D_KEYCLOAK} docker://${TARGET}/quay.io/example/keycloak@${D_KEYCLOAK}" "${CALL_LOG}" \
	|| fail "apply did not copy the tagged image by digest"
rg --quiet 'helm pull widget --repo https://charts.example.com --version 4.5.6' "${CALL_LOG}" \
	|| fail "apply did not pull the classic HTTP chart"
rg --quiet "helm push .*/widget-4.5.6.tgz oci://${TARGET}/charts" "${CALL_LOG}" \
	|| fail "apply did not push the classic HTTP chart to the mirror"
# Two chartSources + one OCI-repo chart + one image + one tagged image = 5 skopeo copies; the
# classic HTTP chart travels through helm pull/push instead.
copy_count="$(rg --count '^skopeo copy ' "${CALL_LOG}" || true)"
[ "${copy_count}" = "5" ] || fail "expected 5 skopeo copies, got ${copy_count}"

# 4. Idempotent re-run against the same mirrored state performs no copy/pull/push and succeeds.
run idem yes "${BOM_OK}" "${STATE_A}"
[ "${RC}" -eq 0 ] || fail "idempotent re-run exited ${RC}"
if rg --quiet '^skopeo copy ' "${CALL_LOG}" || rg --quiet '^helm (pull|push)' "${CALL_LOG}"; then
	fail "idempotent re-run re-copied an already-present artifact"
fi
rg --quiet 'idempotent skip' "${ERR_FILE}" || fail "idempotent re-run did not report a skip"

# 5. A genuine copy failure fails closed non-zero with wrapped context (never swallowed).
readonly STATE_FAIL="${WORK_DIR}/state-fail.list"
: >"${STATE_FAIL}"
run copyfail yes "${BOM_OK}" "${STATE_FAIL}" skopeo-copy
[ "${RC}" -ne 0 ] || fail "copy failure did not fail closed"
rg --quiet 'failed to mirror' "${ERR_FILE}" || fail "copy failure lacked a wrapped diagnostic"

# 6. resolved:false entries fail closed and loudly (never a false 'mirror complete'); the
#    GitRepository is info and is NOT counted as a residual.
run gap no "${BOM_GAP}"
[ "${RC}" -ne 0 ] || fail "a BOM with resolved:false images did not fail closed"
rg --quiet 'mirror incomplete: 2 artifact' "${ERR_FILE}" || fail "did not count exactly the two resolved:false residuals"
rg --quiet 'INFO: chart source git-chart: GitRepository' "${ERR_FILE}" || fail "GitRepository was not treated as info"
rg --quiet 'image 0.72.0.*chart-default image; repository absent' "${ERR_FILE}" || fail "did not surface the resolved:false image note"
rg --quiet 'tagged image tag=2.19.0.*chart-default image; repository absent' "${ERR_FILE}" || fail "did not surface the resolved:false tagged image note"
# The informational GitRepository must never appear as an UNRESOLVED residual.
if rg --quiet 'UNRESOLVED: chart source git-chart' "${ERR_FILE}"; then
	fail "GitRepository was miscounted as an unresolved residual"
fi

# 7. Content-free: no credential ever reaches the script's output or the mirroring calls.
for artifact in "${WORK_DIR}"/*.out "${WORK_DIR}"/*.err "${WORK_DIR}"/*.calls; do
	if rg --quiet --fixed-strings "${REGISTRY_PASSWORD_CANARY}" "${artifact}"; then
		fail "a credential leaked into ${artifact}"
	fi
done

# 8. Argument and flag validation fails closed.
set +e
env -i PATH="${BIN_DIR}:${PATH}" HOME="${WORK_DIR}" bash "${SCRIPT}" >/dev/null 2>&1
missing_rc=$?
env -i PATH="${BIN_DIR}:${PATH}" HOME="${WORK_DIR}" MIRROR_APPLY=maybe MIRROR_BOM_FILE="${BOM_OK}" \
	bash "${SCRIPT}" "${TARGET}" >/dev/null 2>"${WORK_DIR}/badapply.err"
badapply_rc=$?
set -e
[ "${missing_rc}" -eq 2 ] || fail "missing target-registry argument did not exit 2 (got ${missing_rc})"
[ "${badapply_rc}" -ne 0 ] || fail "invalid MIRROR_APPLY did not fail closed"
rg --quiet 'MIRROR_APPLY must be yes or no' "${WORK_DIR}/badapply.err" || fail "invalid MIRROR_APPLY lacked a diagnostic"

echo "mirror-artifacts.sh offline contract tests passed"
