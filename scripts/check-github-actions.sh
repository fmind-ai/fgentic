#!/usr/bin/env bash
# Parse every workflow with yq, then require immutable remote action revisions and a visible
# version hint for Renovate and human reviewers. Local actions remain intentionally unpinned.
set -euo pipefail

readonly root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

for command in rg yq; do
	command -v "${command}" >/dev/null 2>&1 || {
		echo "error: required command not found: ${command}" >&2
		exit 2
	}
done

mapfile -t workflows < <(
	rg --files "${root_dir}/.github/workflows" -g '*.yml' -g '*.yaml' | sort
)
((${#workflows[@]} > 0)) || {
	echo "error: no GitHub Actions workflows found" >&2
	exit 1
}

failed=false
remote_pattern='^[[:alnum:]_.-]+/[[:alnum:]_.-]+(/[[:alnum:]_.-]+)*@[0-9a-f]{40}$'
versioned_line_pattern='^[[:space:]]*uses:[[:space:]]+[[:alnum:]_.-]+/[[:alnum:]_.-]+(/[[:alnum:]_.-]+)*@[0-9a-f]{40}[[:space:]]+#[[:space:]]+v[0-9]+([.][0-9]+){0,2}[[:space:]]*$'

for workflow in "${workflows[@]}"; do
	while IFS= read -r uses; do
		[[ -n "${uses}" ]] || continue
		[[ "${uses}" == ./* ]] && continue
		if [[ ! "${uses}" =~ ${remote_pattern} ]]; then
			echo "error: ${workflow#"${root_dir}/"}: remote action is not pinned to 40 hex: ${uses}" >&2
			failed=true
		fi
	done < <(yq -r '.. | select(tag == "!!map" and has("uses")) | .uses' "${workflow}")

	while IFS= read -r line; do
		[[ "${line}" =~ ^[[:space:]]*uses:[[:space:]]+\./ ]] && continue
		if [[ ! "${line}" =~ ${versioned_line_pattern} ]]; then
			echo "error: ${workflow#"${root_dir}/"}: pinned action needs a '# vN' version hint: ${line}" >&2
			failed=true
		fi
	done < <(rg '^[[:space:]]*uses:' "${workflow}")
done

[[ "${failed}" == false ]] || exit 1
echo "GitHub Actions pinning contract passed (${#workflows[@]} workflows)"
