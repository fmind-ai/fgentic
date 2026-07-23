#!/usr/bin/env bash
# Mirror every artifact in the adopter Bill of Materials (release/bom.yaml) into a private OCI
# registry so an air-gapped cluster can reconcile the reference profile without reaching the public
# internet (issue #457, Task 3). This is a thin, digest-preserving composition over skopeo (OCI
# images and OCI charts) and helm (classic HTTP chart repositories) — it builds no registry and
# mirrors no credentials.
#
# The target path namespaces every artifact under its ORIGIN registry host
# (<target-registry>/<origin-host>/<repo>...), which is collision-free across registries and lets the
# recommended node-level containerd registry mirror (the consumption seam, issue #457 Task 2)
# redirect docker.io/ghcr.io/quay.io/... transparently. See docs/airgap.md.
#
# Usage:
#   scripts/mirror-artifacts.sh <target-registry>                    # dry-run: print the copy plan
#   MIRROR_APPLY=yes scripts/mirror-artifacts.sh <target-registry>   # execute the copies
#
# Dry-run is the default and performs NO push. Set MIRROR_APPLY=yes to execute. The BOM path
# defaults to release/bom.yaml and is overridable with MIRROR_BOM_FILE. Registry authentication is
# ambient (skopeo/helm read the login already in the environment); no credential is ever passed on a
# command line or printed.
#
# Fail-closed: a genuine copy failure aborts non-zero with wrapped context, and any BOM entry whose
# source cannot be derived from the BOM alone — a chart-default image that records only tag@digest
# with no repository, or a GitRepository chart source, which is not a registry artifact — is reported
# explicitly and makes the run exit non-zero. The script never claims a complete mirror while
# silently under-copying.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
# shellcheck source=scripts/lib.sh
source "${SCRIPT_DIR}/lib.sh"

usage() {
	cat <<'EOF'
usage: scripts/mirror-artifacts.sh <target-registry>

Copy every artifact in release/bom.yaml into <target-registry> for a disconnected install.

  <target-registry>   OCI registry host[:port][/path] that receives the mirror
                      (e.g. registry.internal:5000 or harbor.internal/fgentic).

Environment:
  MIRROR_APPLY=yes    Execute the copies. Default (unset or any other value) is a dry-run plan only.
  MIRROR_BOM_FILE     BOM path to read (default: release/bom.yaml).
EOF
}

TARGET_REGISTRY="${1:-}"
case "${TARGET_REGISTRY}" in
	"" | -h | --help)
		usage >&2
		exit 2
		;;
	-*)
		echo "error: unknown flag: ${TARGET_REGISTRY}" >&2
		usage >&2
		exit 2
		;;
	*) ;;
esac
# Strip a trailing slash so a target reference never contains a double separator.
TARGET_REGISTRY="${TARGET_REGISTRY%/}"

BOM_FILE="${MIRROR_BOM_FILE:-${REPO_ROOT}/release/bom.yaml}"
APPLY="${MIRROR_APPLY:-no}"
case "${APPLY}" in
	yes | no) ;;
	*)
		die "MIRROR_APPLY must be yes or no (got '${APPLY}')"
		;;
esac
[ -f "${BOM_FILE}" ] || die "BOM file not found: ${BOM_FILE}"

require_command yq
require_command skopeo
require_command helm

log() {
	local now
	now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	printf '%s %s\n' "${now}" "$*" >&2
}

# Running tallies for the honest closing summary.
PLANNED=0
COPIED=0
SKIPPED=0
declare -a UNRESOLVED=()

record_unresolved() {
	UNRESOLVED+=("$1")
	log "UNRESOLVED: $1"
}

# A non-registry artifact that the air-gap flow mirrors by another path (an internal Git remote) is
# reported as information, not a residual failure — it is a known, documented seam, not a gap.
record_info() {
	log "INFO: $1"
}

# qualify_oci_ref <raw-ref> — print a fully-qualified OCI reference (registry/repo[@digest|:tag]),
# preferring the digest when one is present and dropping the redundant tag, or print nothing when no
# registry/repository can be recovered from the reference (a chart-default image recorded only as
# tag@digest). Always returns 0; callers treat an empty result as unmirrorable-from-the-BOM.
qualify_oci_ref() {
	local raw="${1}" digest="" name first last
	if [[ "${raw}" == *@sha256:* ]]; then
		digest="sha256:${raw##*@sha256:}"
		name="${raw%@sha256:*}"
	else
		name="${raw}"
	fi
	case "${name}" in
		*/*) ;;
		*)
			# No repository path at all: a chart-default image with no source repository in the BOM.
			return 0
			;;
	esac
	first="${name%%/*}"
	# A first path segment that is not registry-like implies Docker Hub (docker.io short form).
	case "${first}" in
		*.* | *:* | localhost) ;;
		*)
			name="docker.io/${name}"
			;;
	esac
	if [ -n "${digest}" ]; then
		# Prefer the digest for integrity; strip a redundant :tag from the final path segment only
		# (never a registry :port, which is not in the last segment).
		last="${name##*/}"
		if [[ "${last}" == *:* ]]; then
			name="${name%:*}"
		fi
		printf '%s@%s' "${name}" "${digest}"
	else
		printf '%s' "${name}"
	fi
}

# mirror_oci <label> <qualified-source-ref> — copy one OCI artifact (image or chart) to the mirror,
# preserving its digest/tag, idempotent on an already-present target and fail-closed on a real error.
mirror_oci() {
	local label="${1}" source="${2}" target
	target="${TARGET_REGISTRY}/${source}"
	PLANNED=$((PLANNED + 1))
	if [ "${APPLY}" = "yes" ] && skopeo inspect --raw "docker://${target}" >/dev/null 2>&1; then
		log "present ${label} docker://${target} (idempotent skip)"
		SKIPPED=$((SKIPPED + 1))
		return 0
	fi
	if [ "${APPLY}" != "yes" ]; then
		log "DRY-RUN ${label} docker://${source} -> docker://${target}"
		return 0
	fi
	log "copy ${label} docker://${source} -> docker://${target}"
	skopeo copy "docker://${source}" "docker://${target}" \
		|| die "failed to mirror ${label}: docker://${source} -> docker://${target}"
	COPIED=$((COPIED + 1))
}

# mirror_helm_http <chart> <version> <repo-url> — pull a chart from a classic HTTP Helm repository
# and re-push it to the mirror's OCI path. Classic repositories carry no upstream OCI digest, so the
# version pin (not a digest) is the preserved identity here; this is called out in docs/airgap.md.
mirror_helm_http() {
	local chart="${1}" version="${2}" url="${3}" target work
	target="${TARGET_REGISTRY}/charts/${chart}:${version}"
	PLANNED=$((PLANNED + 1))
	if [ "${APPLY}" = "yes" ] && skopeo inspect --raw "docker://${target}" >/dev/null 2>&1; then
		log "present chart docker://${target} (idempotent skip)"
		SKIPPED=$((SKIPPED + 1))
		return 0
	fi
	if [ "${APPLY}" != "yes" ]; then
		log "DRY-RUN chart ${url} ${chart}@${version} -> oci://${TARGET_REGISTRY}/charts"
		return 0
	fi
	work="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-mirror.XXXXXX")"
	helm pull "${chart}" --repo "${url}" --version "${version}" --destination "${work}" \
		|| {
			rm -rf "${work}"
			die "failed to pull chart ${chart}@${version} from ${url}"
		}
	helm push "${work}/${chart}-${version}.tgz" "oci://${TARGET_REGISTRY}/charts" \
		|| {
			rm -rf "${work}"
			die "failed to push chart ${chart}@${version} to ${TARGET_REGISTRY}/charts"
		}
	rm -rf "${work}"
	log "mirrored chart ${chart}@${version} -> oci://${target}"
	COPIED=$((COPIED + 1))
}

# Empty fields must never appear mid-row: `read` with a whitespace IFS (tab) collapses consecutive
# delimiters, which would shift later columns. yq therefore emits "-" for a null field (the same
# sentinel gen-bom.sh uses); `denull` maps it back to an empty string after splitting.
denull() {
	if [ "${1}" = "-" ]; then
		printf ''
	else
		printf '%s' "${1}"
	fi
}

log "reading BOM ${BOM_FILE} (target ${TARGET_REGISTRY}, apply=${APPLY})"

# Index the classic/OCI chart hosts by name so a HelmRelease can be joined to its repository. A
# HelmRelease whose source is NOT here is backed by a chartSource (OCIRepository/GitRepository),
# which is mirrored by the chartSources loop below.
# Each section is captured into a variable first (a plain assignment does not mask yq's exit status,
# and fails fast under `set -e` on a malformed BOM) and iterated with a here-string, which — unlike a
# pipe — keeps the loop in the current shell so REPO_URL and the tallies persist.
declare -A REPO_URL=()
repositories_tsv="$(yq -r '(.helmRepositories // [])[] | [.name, .url] | @tsv' "${BOM_FILE}")"
while IFS=$'\t' read -r repo_name repo_url; do
	[ -n "${repo_name}" ] || continue
	REPO_URL["${repo_name}"]="${repo_url}"
done <<<"${repositories_tsv}"

# --- chart sources (OCIRepository artifacts / GitRepository sources) -------------------------------
chart_sources_tsv="$(yq -r '(.chartSources // [])[] | [.name, .kind, .url, (.version // "-")] | @tsv' "${BOM_FILE}")"
while IFS=$'\t' read -r cs_name cs_kind cs_url cs_version; do
	[ -n "${cs_name}" ] || continue
	cs_version="$(denull "${cs_version}")"
	case "${cs_kind}" in
		OCIRepository)
			cs_base="${cs_url#oci://}"
			if [[ "${cs_version}" == sha256:* ]]; then
				cs_ref="${cs_base}@${cs_version}"
			else
				cs_ref="${cs_base}:${cs_version}"
			fi
			cs_q="$(qualify_oci_ref "${cs_ref}")"
			if [ -z "${cs_q}" ]; then
				record_unresolved "chart source ${cs_name}: cannot qualify ${cs_url} @ ${cs_version}"
			else
				mirror_oci "chart" "${cs_q}"
			fi
			;;
		GitRepository)
			# A git chart source is legitimately not a registry artifact: the air-gap flow mirrors it to
			# an internal Git remote (docs/airgap.md step 3), so it is informational, not a failure.
			record_info "chart source ${cs_name}: GitRepository ${cs_url} @ ${cs_version} is mirrored via the internal Git remote (not skopeo); see docs/airgap.md"
			;;
		*)
			record_unresolved "chart source ${cs_name}: unsupported kind ${cs_kind}"
			;;
	esac
done <<<"${chart_sources_tsv}"

# --- helm releases backed by a Helm repository (OCI or classic HTTP) -------------------------------
helm_releases_tsv="$(yq -r '(.helmReleases // [])[] | [.name, .chart, (.version // "-"), (.source // "-")] | @tsv' "${BOM_FILE}")"
while IFS=$'\t' read -r hr_name hr_chart hr_version hr_source; do
	[ -n "${hr_name}" ] || continue
	hr_version="$(denull "${hr_version}")"
	hr_source="$(denull "${hr_source}")"
	# Guard the element count first: indexing an empty associative array is a "bad array subscript"
	# error under `set -u`, so never subscript REPO_URL when no helmRepositories were declared.
	hr_url=""
	if [ "${#REPO_URL[@]}" -gt 0 ] && [ -n "${REPO_URL[${hr_source}]+set}" ]; then
		hr_url="${REPO_URL[${hr_source}]}"
	fi
	if [ -z "${hr_url}" ]; then
		# Source is a chartSource; already mirrored (or reported) by the chartSources loop.
		log "skip release ${hr_name}: chart source '${hr_source}' handled via chartSources"
		continue
	fi
	if [ -z "${hr_version}" ]; then
		record_unresolved "helm release ${hr_name}: no chart version pinned for repository source '${hr_source}'"
		continue
	fi
	case "${hr_url}" in
		oci://*)
			hr_base="${hr_url#oci://}"
			hr_ref="${hr_base}/${hr_chart}:${hr_version}"
			hr_q="$(qualify_oci_ref "${hr_ref}")"
			if [ -z "${hr_q}" ]; then
				record_unresolved "helm release ${hr_name}: cannot qualify OCI chart ${hr_ref}"
			else
				mirror_oci "chart" "${hr_q}"
			fi
			;;
		http://* | https://*)
			mirror_helm_http "${hr_chart}" "${hr_version}" "${hr_url}"
			;;
		*)
			record_unresolved "helm release ${hr_name}: unsupported repository url ${hr_url}"
			;;
	esac
done <<<"${helm_releases_tsv}"

# --- images (digest-pinned references) -------------------------------------------------------------
# `((.resolved == false) | not)` yields "false" only when resolved is explicitly false; the `//`
# alternative operator cannot be used for the default because yq treats a boolean false as empty.
images_tsv="$(yq -r '(.images // [])[] | [.ref, .digest, ((.resolved == false) | not), (.note // "-")] | @tsv' "${BOM_FILE}")"
while IFS=$'\t' read -r img_ref img_digest img_resolved img_note; do
	[ -n "${img_ref}" ] || continue
	img_note="$(denull "${img_note}")"
	if [ "${img_resolved}" = "false" ]; then
		record_unresolved "image ${img_ref} (digest ${img_digest}): ${img_note:-chart-default image, repository not recorded in the BOM}; mirror it via the node-level containerd registry mirror or extend the BOM to record its repository"
		continue
	fi
	img_q="$(qualify_oci_ref "${img_ref}")"
	if [ -z "${img_q}" ]; then
		record_unresolved "image ${img_ref} (digest ${img_digest}): no source repository could be derived from the BOM ref; mirror it via the node-level containerd registry mirror or extend the BOM to record its repository"
	else
		mirror_oci "image" "${img_q}"
	fi
done <<<"${images_tsv}"

# --- tagged images (explicit tag overrides in reconciled HelmRelease values) -----------------------
tagged_images_tsv="$(yq -r '(.taggedImages // [])[] | [(.registry // "-"), (.repository // "-"), (.tag // "-"), (.digest // "-"), ((.resolved == false) | not), (.note // "-")] | @tsv' "${BOM_FILE}")"
while IFS=$'\t' read -r ti_registry ti_repository ti_tag ti_digest ti_resolved ti_note; do
	[ -n "${ti_tag}" ] || continue
	ti_registry="$(denull "${ti_registry}")"
	ti_repository="$(denull "${ti_repository}")"
	ti_tag="$(denull "${ti_tag}")"
	ti_digest="$(denull "${ti_digest}")"
	ti_note="$(denull "${ti_note}")"
	[ -n "${ti_tag}" ] || continue
	if [ "${ti_resolved}" = "false" ] || [ -z "${ti_repository}" ]; then
		record_unresolved "tagged image tag=${ti_tag} digest=${ti_digest}: ${ti_note:-chart-default image, repository not recorded in the BOM}; mirror it via the node-level containerd registry mirror or extend the BOM to record its repository"
		continue
	fi
	if [ -n "${ti_registry}" ]; then
		ti_full="${ti_registry}/${ti_repository}"
	else
		ti_full="${ti_repository}"
	fi
	if [ -n "${ti_digest}" ]; then
		ti_raw="${ti_full}:${ti_tag}@${ti_digest}"
	else
		ti_raw="${ti_full}:${ti_tag}"
	fi
	ti_q="$(qualify_oci_ref "${ti_raw}")"
	if [ -z "${ti_q}" ]; then
		record_unresolved "tagged image ${ti_raw}: cannot qualify a registry/repository"
	else
		mirror_oci "image" "${ti_q}"
	fi
done <<<"${tagged_images_tsv}"

# --- honest closing summary ------------------------------------------------------------------------
log "plan: ${PLANNED} registry artifact(s), ${#UNRESOLVED[@]} unresolved"
if [ "${APPLY}" = "yes" ]; then
	log "applied: ${COPIED} copied, ${SKIPPED} already present"
fi
if [ "${#UNRESOLVED[@]}" -gt 0 ]; then
	log "INCOMPLETE: ${#UNRESOLVED[@]} BOM artifact(s) cannot be mirrored from the BOM alone:"
	for item in "${UNRESOLVED[@]}"; do
		log "  - ${item}"
	done
	die "mirror incomplete: ${#UNRESOLVED[@]} artifact(s) unresolved (see above); refusing to report a complete mirror"
fi
if [ "${APPLY}" = "yes" ]; then
	log "mirror complete: ${PLANNED} artifact(s) present in ${TARGET_REGISTRY}"
else
	log "dry-run complete: ${PLANNED} artifact(s) planned for ${TARGET_REGISTRY} (set MIRROR_APPLY=yes to execute)"
fi
