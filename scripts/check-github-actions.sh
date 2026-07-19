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
	# Every workflow must make duplicate-run behavior explicit. This prevents unbounded
	# parallel CI work and keeps stateful release/proof workflows serialized by design.
	concurrency_rows="$(
		yq -o=json '.concurrency' "${workflow}" | jq -r '
		  def github_expression:
		    if type == "string" then test("^\\$\\{\\{.+\\}\\}$") else false end;
		  . as $concurrency |
		  if ($concurrency | type) != "object" then
		    ["concurrency", ($concurrency | tojson)]
		  else
		    (if ($concurrency | has("group") | not) then
		      ["concurrency.group", "<missing>"]
		    elif (($concurrency.group | type) != "string" or ($concurrency.group | length) == 0) then
		      ["concurrency.group", ($concurrency.group | tojson)]
		    else empty end),
		    (if ($concurrency | has("cancel-in-progress") | not) then
		      ["concurrency.cancel-in-progress", "<missing>"]
		    elif (($concurrency["cancel-in-progress"] | type) == "boolean" or
		      ($concurrency["cancel-in-progress"] | github_expression)) then
		      empty
		    else
		      ["concurrency.cancel-in-progress", ($concurrency["cancel-in-progress"] | tojson)]
		    end)
		  end |
		  @tsv
		'
	)"
	while IFS=$'\t' read -r field value; do
		[[ -n "${field}" ]] || continue
		echo "error: ${workflow#"${root_dir}/"}: invalid ${field}; got ${value}" >&2
		failed=true
	done <<<"${concurrency_rows}"

	uses_list="$(yq -r '.. | select(tag == "!!map" and has("uses")) | .uses' "${workflow}")"
	while IFS= read -r uses; do
		[[ -n "${uses}" ]] || continue
		[[ "${uses}" == ./* ]] && continue
		if [[ ! "${uses}" =~ ${remote_pattern} ]]; then
			echo "error: ${workflow#"${root_dir}/"}: remote action is not pinned to 40 hex: ${uses}" >&2
			failed=true
		fi
	done <<<"${uses_list}"

	# checkout persists its token in local Git configuration by default. Require the native
	# boolean false on every checkout step, independent of the action's Renovate-managed digest.
	checkout_rows="$(
		yq -o=json '.jobs' "${workflow}" | jq -r '
		  def has_persist_credentials:
		    if (.with | type) == "object"
		      then (.with | has("persist-credentials"))
		      else false
		    end;
		  to_entries[] as $job |
		  ($job.value.steps // [] | to_entries[]) as $step |
		  $step.value |
		  select((.uses | type) == "string") |
		  select(.uses | ascii_downcase | startswith("actions/checkout@")) |
		  [
		    ($job.key + ".steps[" + ($step.key | tostring) + "]"),
		    (if has_persist_credentials
		      then (.with["persist-credentials"] | tojson)
		      else "<missing>"
		    end),
		    (if has_persist_credentials
		      then ((.with["persist-credentials"] | type) == "boolean" and
		        .with["persist-credentials"] == false)
		      else false
		    end)
		  ] |
		  @tsv
		'
	)"
	while IFS=$'\t' read -r step credentials valid; do
		[[ -n "${step}" ]] || continue
		if [[ "${valid}" != true ]]; then
			echo "error: ${workflow#"${root_dir}/"}: checkout step ${step} needs persist-credentials: false; got ${credentials}" >&2
			failed=true
		fi
	done <<<"${checkout_rows}"

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

	# GitHub otherwise lets a job occupy a runner for up to six hours. Keep every job's
	# ceiling explicit, finite, and reviewable without hard-coding the current inventory.
	timeout_rows="$(
		yq -o=json '.jobs' "${workflow}" | jq -r '
		  to_entries[] |
		  . as $job |
		  ($job.value["timeout-minutes"]) as $timeout |
		  [
		    $job.key,
		    (if $job.value | has("timeout-minutes") then ($timeout | tojson) else "<missing>" end),
		    (if ($timeout | type) == "number"
		      then (($timeout | floor) == $timeout and $timeout > 0 and $timeout < 360)
		      else false
		    end)
		  ] |
		  @tsv
		'
	)"
	while IFS=$'\t' read -r job timeout valid; do
		[[ -n "${job}" ]] || continue
		if [[ "${valid}" != true ]]; then
			echo "error: ${workflow#"${root_dir}/"}: job ${job} needs an integer timeout-minutes between 1 and 359; got ${timeout}" >&2
			failed=true
		fi
	done <<<"${timeout_rows}"
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
echo "GitHub Actions pinning, checkout-hardening, bounded-runtime, concurrency, and serialized-install contracts passed (${#workflows[@]} workflows)"
