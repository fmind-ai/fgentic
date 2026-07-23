#!/usr/bin/env bash
# Generate the adopter Bill of Materials (issue #188): every pinned chart source, HelmRelease
# chart version, and digest-pinned container image reconciled by the reference release profile.
#
# The output is deterministic (every list is sorted, no timestamp is embedded) and canonicalized
# through `dprint fmt` so `scripts/check-bom.sh` can regenerate-and-diff it. A tagged release =
# this exact pin-set; see docs/releases.md. Regenerate with `bash scripts/gen-bom.sh`; the
# committed artifact is verified by `mise run check:bom`.
#
# Usage: scripts/gen-bom.sh [--stdout]   (--stdout prints to stdout instead of writing the file)
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
# shellcheck source=scripts/lib/bom-scope.sh
source "${script_dir}/lib/bom-scope.sh"

readonly OUTPUT_FILE="release/bom.yaml"

for command in yq rg dprint; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		echo "error: required command not found: ${command}" >&2
		exit 2
	fi
done

cd "${repo_root}"

emit_to_stdout="no"
case "${1:-}" in
	--stdout)
		emit_to_stdout="yes"
		;;
	"")
		;;
	*)
		echo "error: unknown argument: ${1}" >&2
		exit 2
		;;
esac

# awk helper shared by every list emitter: quote a scalar, rendering empty/"-" as YAML null.
readonly AWK_SCALAR='function scalar(v) { return (v == "" || v == "-") ? "null" : "\"" v "\"" }'

# The note attached to an image whose repository is a chart default that is not present in the repo
# manifests: it cannot be fully qualified (or mirrored) offline without rendering the chart.
readonly IMAGE_UNRESOLVED_NOTE="chart-default image; repository absent from repo manifests — enumerate with helm template before mirroring"

# --- chart sources (OCIRepository / GitRepository pinned artifacts) --------------------------------
chart_sources="$(
	for file in "${BOM_CHART_FILES[@]}"; do
		yq -er '
      select(.kind == "OCIRepository" or .kind == "GitRepository")
      | [.metadata.name, .kind, .spec.url, (.spec.ref.tag // .spec.ref.commit // .spec.ref.digest // "-")]
      | @tsv
    ' "${file}" 2>/dev/null | while IFS=$'\t' read -r name kind url version; do
			[ -n "${name}" ] || continue # skip empty lines emitted by non-matching docs
			printf '%s\t%s\t%s\t%s\t%s\n' "${name}" "${kind}" "${url}" "${version}" "${file}"
		done
	done | LC_ALL=C sort -u
)"

# --- helm releases (chart version pins) -----------------------------------------------------------
helm_releases="$(
	for file in "${BOM_CHART_FILES[@]}"; do
		yq -er '
      select(.kind == "HelmRelease")
      | [.metadata.name, (.metadata.namespace // "-"),
         (.spec.chart.spec.chart // .spec.chartRef.name // "-"),
         (.spec.chart.spec.version // "-"),
         (.spec.chart.spec.sourceRef.name // .spec.chartRef.name // "-")]
      | @tsv
    ' "${file}" 2>/dev/null | while IFS=$'\t' read -r name namespace chart version source; do
			[ -n "${name}" ] || continue # skip empty lines emitted by non-matching docs
			printf '%s\t%s\t%s\t%s\t%s\t%s\n' "${name}" "${namespace}" "${chart}" "${version}" "${source}" "${file}"
		done
	done | LC_ALL=C sort -u
)"

# --- helm repositories (chart hosts without an in-file version pin) --------------------------------
helm_repositories="$(
	for file in "${BOM_CHART_FILES[@]}"; do
		yq -er '
      select(.kind == "HelmRepository") | [.metadata.name, .spec.url] | @tsv
    ' "${file}" 2>/dev/null | while IFS=$'\t' read -r name url; do
			[ -n "${name}" ] || continue # skip empty lines emitted by non-matching docs
			printf '%s\t%s\t%s\n' "${name}" "${url}" "${file}"
		done
	done | LC_ALL=C sort -u
)"

# --- images (digest-pinned container images) ------------------------------------------------------
# Every image is keyed by its authoritative, globally-unique sha256 digest. When the repository (and
# registry) can be reconstructed from the sibling `registry:`/`repository:` keys of a HelmRelease
# image map, `ref` is the COMPLETE, mirrorable reference `[registry/]repository:tag@sha256:...` and
# `resolved: true`. An image whose repository is a chart default absent from the repo manifests keeps
# the bare `tag@sha256:` token, is marked `resolved: false`, and carries a `note:` — it cannot be
# fully qualified or mirrored offline without a `helm template` render (see docs/airgap.md). Plain
# inline `image:`/`imageName:` strings outside a HelmRelease are already complete references.
structured_images="$(
	for file in "${BOM_CHART_FILES[@]}"; do
		yq -er '
      select(.kind == "HelmRelease") | .spec.values | .. | select(tag == "!!map" and has("tag"))
      | select(.tag | tostring | test("@sha256:"))
      | [(.registry // "-"), (.repository // "-"), (.tag | tostring)] | @tsv
    ' "${file}" 2>/dev/null | while IFS=$'\t' read -r registry repository tag; do
			[ -n "${tag}" ] || continue # skip empty lines emitted by non-matching docs
			digest="sha256:${tag##*@sha256:}"
			if [ "${repository}" != "-" ]; then
				if [ "${registry}" != "-" ]; then
					ref="${registry}/${repository}:${tag}"
				else
					ref="${repository}:${tag}"
				fi
				printf '%s\t%s\t%s\t%s\t%s\n' "${ref}" "${digest}" "true" "-" "${file}"
			else
				printf '%s\t%s\t%s\t%s\t%s\n' "${tag}" "${digest}" "false" "${IMAGE_UNRESOLVED_NOTE}" "${file}"
			fi
		done
	done
)"
# Digests already captured (with their repository) as a structured HelmRelease image, so the inline
# scan does not re-emit them as a bare, repository-less duplicate.
structured_digests="$(printf '%s\n' "${structured_images}" | awk -F'\t' 'NF > 0 { print $2 }' | LC_ALL=C sort -u)"
inline_images="$(
	for file in "${BOM_IMAGE_FILES[@]}"; do
		rg -oN "[^[:space:]\"']+@sha256:[0-9a-f]{64}" "${file}" | while read -r ref; do
			digest="sha256:${ref##*@sha256:}"
			if printf '%s\n' "${structured_digests}" | grep -qxF "${digest}"; then
				continue
			fi
			printf '%s\t%s\t%s\t%s\t%s\n' "${ref}" "${digest}" "true" "-" "${file}"
		done
	done
)"
image_rows="$(printf '%s\n%s\n' "${structured_images}" "${inline_images}" | grep -v '^[[:space:]]*$' | LC_ALL=C sort -u)"

# --- tagged images (explicit tag overrides in reconciled HelmRelease values) -----------------------
# Some reconciled charts pin an image through an explicit `image:` value override (registry /
# repository / tag, with an optional sibling `digest:` key) rather than an inline `tag@sha256:`
# reference. An air-gap adopter mirroring strictly from the BOM would miss these unless they are
# enumerated. Scan every in-scope HelmRelease's `spec.values` for a map that carries a `tag` key
# whose value is NOT an inline digest (those are already covered by `images:` above). Repository or
# registry may be absent when the chart supplies them by default; the recorded tag (and digest when
# present) is what makes the image mirrorable and pin-verifiable.
tagged_image_rows="$(
	for file in "${BOM_CHART_FILES[@]}"; do
		yq -er '
      select(.kind == "HelmRelease") | .spec.values | .. | select(tag == "!!map" and has("tag"))
      | select((.tag | tostring | test("@sha256:")) | not)
      | [(.registry // "-"), (.repository // "-"), (.tag | tostring), (.digest // "-")] | @tsv
    ' "${file}" 2>/dev/null | while IFS=$'\t' read -r registry repository tag digest; do
			[ -n "${tag}" ] || continue # skip empty lines emitted by non-matching maps
			# `resolved` mirrors the images rule: a repository present in the values makes the image
			# fully qualified and mirrorable; a chart-default repository (absent) does not.
			if [ "${repository}" != "-" ]; then
				printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "${registry}" "${repository}" "${tag}" "${digest}" "true" "-" "${file}"
			else
				printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "${registry}" "${repository}" "${tag}" "${digest}" "false" "${IMAGE_UNRESOLVED_NOTE}" "${file}"
			fi
		done
	done | LC_ALL=C sort -u
)"

build_bom() {
	cat <<'HEADER'
# Adopter Bill of Materials for the Fgentic reference release profile (issue #188).
#
# GENERATED by scripts/gen-bom.sh — do NOT edit by hand. Regenerate with `bash scripts/gen-bom.sh`
# and verify with `mise run check:bom`, which fails closed on any in-scope pin missing here and on
# any repo pin that is neither listed here nor in the exclusion allowlist (scripts/lib/bom-scope.sh).
#
# A tagged release == this exact pin-set. Scope: the reconciled clusters/local + clusters/gcp Flux
# DAG under infra/ plus apps/matrix-a2a-bridge/deploy, with the tracked default platform-settings.
# `images:` holds digest-pinned references; `taggedImages:` holds images pinned by an explicit tag
# override in the referenced HelmRelease values (with a sibling digest when present). Each image and
# tagged image carries `resolved:` — `true` when `ref`/`repository` is a complete, mirrorable
# reference reconstructed from the manifests, `false` (with a `note:`) when the repository is a chart
# default absent from the repo and only a `helm template` render can qualify it (see docs/airgap.md).
# Some `helmRepositories:` back opt-in profiles (e.g. `vllm`) not reconciled by the vertex default.
# See docs/releases.md.
apiVersion: fgentic.dev/bom/v1
kind: BillOfMaterials
metadata:
  generatedBy: scripts/gen-bom.sh
  verifiedBy: scripts/check-bom.sh
  profile: reference (clusters/local + clusters/gcp, default platform-settings)
HEADER

	printf 'chartSources:\n'
	if [ -n "${chart_sources}" ]; then
		printf '%s\n' "${chart_sources}" | awk -F'\t' "${AWK_SCALAR}"'
      { printf "  - name: %s\n    kind: %s\n    url: %s\n    version: %s\n    file: %s\n", scalar($1), scalar($2), scalar($3), scalar($4), scalar($5) }
    '
	else
		printf '  []\n'
	fi

	printf 'helmReleases:\n'
	if [ -n "${helm_releases}" ]; then
		printf '%s\n' "${helm_releases}" | awk -F'\t' "${AWK_SCALAR}"'
      { printf "  - name: %s\n    namespace: %s\n    chart: %s\n    version: %s\n    source: %s\n    file: %s\n", scalar($1), scalar($2), scalar($3), scalar($4), scalar($5), scalar($6) }
    '
	else
		printf '  []\n'
	fi

	printf 'helmRepositories:\n'
	if [ -n "${helm_repositories}" ]; then
		printf '%s\n' "${helm_repositories}" | awk -F'\t' "${AWK_SCALAR}"'
      { printf "  - name: %s\n    url: %s\n    file: %s\n", scalar($1), scalar($2), scalar($3) }
    '
	else
		printf '  []\n'
	fi

	printf 'images:\n'
	if [ -n "${image_rows}" ]; then
		# Rows arrive pre-sorted (LC_ALL=C sort -u), so refs are grouped and their files are already
		# sorted and adjacent; stream them, emitting one entry per ref with its resolved flag, optional
		# note, and deduplicated file list. Fields: ref, digest, resolved, note, file.
		printf '%s\n' "${image_rows}" | awk -F'\t' '
      function flush() {
        if (cur == "") return
        printf "  - ref: \"%s\"\n", cur
        printf "    digest: \"%s\"\n", curdigest
        printf "    resolved: %s\n", curresolved
        if (curnote != "-") printf "    note: \"%s\"\n", curnote
        printf "    files:\n"
        for (i = 1; i <= nfiles; i++) printf "      - %s\n", flist[i]
      }
      {
        if ($1 != cur) { flush(); cur = $1; curdigest = $2; curresolved = $3; curnote = $4; nfiles = 0; prevfile = "" }
        if ($5 != prevfile) { flist[++nfiles] = $5; prevfile = $5 }
      }
      END { flush() }
    '
	else
		printf '  []\n'
	fi

	printf 'taggedImages:\n'
	if [ -n "${tagged_image_rows}" ]; then
		# Fields: registry, repository, tag, digest, resolved, note, file.
		printf '%s\n' "${tagged_image_rows}" | awk -F'\t' "${AWK_SCALAR}"'
      {
        printf "  - registry: %s\n    repository: %s\n    tag: %s\n    digest: %s\n    resolved: %s\n", scalar($1), scalar($2), scalar($3), scalar($4), $5
        if ($6 != "-") printf "    note: \"%s\"\n", $6
        printf "    file: %s\n", scalar($7)
      }
    '
	else
		printf '  []\n'
	fi
}

# `--stdin bom.yaml` only hints the formatter's language by extension; dprint reads the piped
# content, never the named path. Stage to a temp file and move it into place so the output is never
# both the read hint and the write target.
if [ "${emit_to_stdout}" = "yes" ]; then
	build_bom | dprint fmt --stdin bom.yaml
else
	mkdir -p "$(dirname "${OUTPUT_FILE}")"
	staged="$(mktemp --suffix=.yaml)"
	build_bom | dprint fmt --stdin bom.yaml >"${staged}"
	mv "${staged}" "${OUTPUT_FILE}"
	echo "wrote ${OUTPUT_FILE}"
fi
