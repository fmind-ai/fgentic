#!/usr/bin/env bash
# Verify the adopter Bill of Materials (issue #188) — fail closed on drift.
#
# Three assertions, all fail-closed:
#   1. The committed release/bom.yaml equals scripts/gen-bom.sh's current output (regenerate + diff),
#      so the BOM can never drift from the reconciled reference profile it claims to describe.
#   2. Every in-scope digest-pinned image appears in the BOM.
#   3. Every pin-bearing file in the repo is classified: in-scope (covered by the BOM) or matched by
#      the documented exclusion allowlist. A NEW pin that is neither fails the gate — this is the
#      whole point: an incomplete BOM that silently under-covers would give false supply-chain
#      confidence.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
# shellcheck source=scripts/lib/bom-scope.sh
source "${script_dir}/lib/bom-scope.sh"

readonly BOM_FILE="release/bom.yaml"

for command in yq rg dprint git; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		echo "error: required command not found: ${command}" >&2
		exit 2
	fi
done

cd "${repo_root}"

fail=0
note_failure() {
	echo "error: $1" >&2
	fail=1
}

if [ ! -f "${BOM_FILE}" ]; then
	echo "error: ${BOM_FILE} is missing; run 'bash scripts/gen-bom.sh'" >&2
	exit 1
fi

# 1) Regenerate-and-diff: the committed BOM must equal the current generator output.
regenerated="$(mktemp --suffix=.yaml)"
trap 'rm -f "${regenerated}"' EXIT
bash scripts/gen-bom.sh --stdout >"${regenerated}"
if ! diff -u "${BOM_FILE}" "${regenerated}" >/dev/null; then
	note_failure "${BOM_FILE} is stale; regenerate with 'bash scripts/gen-bom.sh'"
	diff -u "${BOM_FILE}" "${regenerated}" >&2 || true
fi

# 2+3) Census enforcement: classify every pin-bearing file, assert in-scope images are covered.
image_pin_files="$(rg -l --sort path -e '@sha256:[0-9a-f]{64}' infra apps clusters)"
while IFS= read -r file; do
	[ -n "${file}" ] || continue
	if bom_is_in_scope "${file}"; then
		refs="$(rg -oN "[^[:space:]\"']+@sha256:[0-9a-f]{64}" "${file}" || true)"
		while read -r ref; do
			[ -n "${ref}" ] || continue
			digest="sha256:${ref##*@sha256:}"
			if ! rg -qF "${digest}" "${BOM_FILE}"; then
				note_failure "in-scope image digest ${digest} (${file}) is missing from ${BOM_FILE}"
			fi
		done <<<"${refs}"
	elif bom_exclusion_reason "${file}" >/dev/null; then
		: # excluded with a documented reason
	else
		note_failure "unclassified image pin file ${file}: add it to scripts/lib/bom-scope.sh (in-scope) or the exclusion allowlist"
	fi
done <<<"${image_pin_files}"

# Tagged-image enforcement: re-derive every explicit image tag override in a reconciled in-scope
# HelmRelease's values and assert each appears in the BOM's taggedImages, exactly like digests. A
# NEW tag override added later therefore fails the gate unless it is captured in the BOM.
for file in "${BOM_CHART_FILES[@]}"; do
	tagged="$(
		yq -er '
      select(.kind == "HelmRelease") | .spec.values | .. | select(tag == "!!map" and has("tag"))
      | select((.tag | tostring | test("@sha256:")) | not)
      | [(.tag | tostring), (.digest // "-")] | @tsv
    ' "${file}" 2>/dev/null || true
	)"
	while IFS=$'\t' read -r tag digest; do
		# Skip blank lines and the `---` document separators yq prints between multiple docs.
		{ [ -n "${tag}" ] && [ "${tag}" != "---" ]; } || continue
		if ! rg -qF -- "${tag}" "${BOM_FILE}"; then
			note_failure "tag-pinned image tag ${tag} (${file}) is missing from taggedImages in ${BOM_FILE}"
		fi
		if [ "${digest}" != "-" ] && ! rg -qF -- "${digest}" "${BOM_FILE}"; then
			note_failure "tag-pinned image digest ${digest} (${file}) is missing from taggedImages in ${BOM_FILE}"
		fi
	done <<<"${tagged}"
done

# Chart-defining files (HelmRelease / OCIRepository / HelmRepository / GitRepository docs).
chart_pin_files="$(rg -l --sort path -e '^apiVersion: helm\.toolkit\.fluxcd\.io' -e '^apiVersion: source\.toolkit\.fluxcd\.io' infra apps clusters)"
while IFS= read -r file; do
	[ -n "${file}" ] || continue
	if bom_is_in_scope "${file}"; then
		: # chart content is covered by the regenerate-and-diff assertion above
	elif bom_exclusion_reason "${file}" >/dev/null; then
		: # excluded with a documented reason
	else
		note_failure "unclassified chart pin file ${file}: add it to scripts/lib/bom-scope.sh (in-scope) or the exclusion allowlist"
	fi
done <<<"${chart_pin_files}"

if [ "${fail}" -ne 0 ]; then
	echo "check:bom FAILED" >&2
	exit 1
fi

echo "check:bom OK — BOM matches the reconciled reference profile; every repo pin is classified"
