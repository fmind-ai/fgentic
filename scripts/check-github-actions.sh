#!/usr/bin/env bash
# Parse every workflow with yq, then require immutable remote action revisions and a visible
# version hint for Renovate and human reviewers. Local actions remain intentionally unpinned.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir

for command in jq mise rg yq; do
	command -v "${command}" >/dev/null 2>&1 || {
		echo "error: required command not found: ${command}" >&2
		exit 2
	}
done

workflow_status=0
workflow_list="$(rg --files "${root_dir}/.github/workflows" -g '*.yml' -g '*.yaml' | sort)" \
	|| workflow_status=$?
((workflow_status <= 1)) || {
	echo "error: could not enumerate GitHub Actions workflows" >&2
	exit "${workflow_status}"
}
[[ -n "${workflow_list}" ]] || {
	echo "error: no GitHub Actions workflows found" >&2
	exit 1
}
mapfile -t workflows <<<"${workflow_list}"

failed=false
remote_pattern='^[[:alnum:]_.-]+/[[:alnum:]_.-]+(/[[:alnum:]_.-]+)*@[0-9a-f]{40}$'
versioned_line_pattern='^[[:space:]]*uses:[[:space:]]+[[:alnum:]_.-]+/[[:alnum:]_.-]+(/[[:alnum:]_.-]+)*@[0-9a-f]{40}[[:space:]]+#[[:space:]]+v[0-9]+([.][0-9]+){0,2}[[:space:]]*$'

for workflow in "${workflows[@]}"; do
	uses_list="$(yq -r '.. | select(tag == "!!map" and has("uses")) | .uses' "${workflow}")"
	while IFS= read -r uses; do
		[[ -n "${uses}" ]] || continue
		[[ "${uses}" == ./* ]] && continue
		if [[ ! "${uses}" =~ ${remote_pattern} ]]; then
			echo "error: ${workflow#"${root_dir}/"}: remote action is not pinned to 40 hex: ${uses}" >&2
			failed=true
		fi
	done <<<"${uses_list}"

	uses_status=0
	uses_lines="$(rg '^[[:space:]]*uses:' "${workflow}")" || uses_status=$?
	((uses_status <= 1)) || {
		echo "error: could not inspect action references in ${workflow}" >&2
		exit "${uses_status}"
	}
	while IFS= read -r line; do
		[[ -n "${line}" ]] || continue
		[[ "${line}" =~ ^[[:space:]]*uses:[[:space:]]+\./ ]] && continue
		if [[ ! "${line}" =~ ${versioned_line_pattern} ]]; then
			echo "error: ${workflow#"${root_dir}/"}: pinned action needs a '# vN' version hint: ${line}" >&2
			failed=true
		fi
	done <<<"${uses_lines}"
done

# Runners must be pinned to an explicit Ubuntu image. `ubuntu-latest` silently re-points over a
# 1-2 month rollout when a new LTS goes GA, which is exactly the unplanned OS flip the host-sensitive
# smoke/policy jobs (Docker/k3d/kind/Calico, kernel-dependent NetworkPolicy tests) cannot absorb (#480).
for workflow in "${workflows[@]}"; do
	if rg -q 'runs-on:[[:space:]]*ubuntu-latest' "${workflow}"; then
		echo "error: ${workflow#"${root_dir}/"}: pin the runner to an explicit ubuntu-<version>, not ubuntu-latest (#480)" >&2
		failed=true
	fi
done

if ! mise --cd "${root_dir}" tasks info install:apps --json | jq -e '
  .depends == [] and
  .run == [
    "mise run install:bridge",
    "mise run install:gateway",
    "mise run install:policy"
  ]
' >/dev/null; then
	echo "error: install:apps must serialize shared mise toolchain installation" >&2
	exit 1
fi

if ! yq --exit-status \
	'.jobs.check.steps[] | select(.run == "mise run install:apps")' \
	"${root_dir}/.github/workflows/ci.yml" >/dev/null; then
	echo "error: CI check job must use the canonical install:apps task" >&2
	exit 1
fi

[[ "${failed}" == false ]] || exit 1
echo "GitHub Actions pinning and serialized-install contracts passed (${#workflows[@]} workflows)"
