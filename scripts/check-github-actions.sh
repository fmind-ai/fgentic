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
	# Every workflow must opt out of repository-default token scopes. Job overrides are
	# optional, but any declared permissions must remain an explicit, reviewable map.
	permission_rows="$(
		yq -o=json '.' "${workflow}" | jq -r '
		  . as $workflow |
		  (if ($workflow | has("permissions") | not) then
		    ["permissions", "<missing>"]
		  elif ($workflow.permissions | type) != "object" then
		    ["permissions", ($workflow.permissions | tojson)]
		  else empty end),
		  ($workflow.jobs | to_entries[] as $job |
		    select(($job.value | type) == "object" and ($job.value | has("permissions"))) |
		    select(($job.value.permissions | type) != "object") |
		    [($job.key + ".permissions"), ($job.value.permissions | tojson)]) |
		  @tsv
		'
	)"
	while IFS=$'\t' read -r field value; do
		[[ -n "${field}" ]] || continue
		echo "error: ${workflow#"${root_dir}/"}: invalid ${field}; expected a permissions map, got ${value}" >&2
		failed=true
	done <<<"${permission_rows}"

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

	# Actions and runner pins do not cover job or service containers. Keep every declared
	# image immutable, including the job-container string shorthand.
	container_rows="$(
		yq -o=json '.jobs' "${workflow}" | jq -r '
		  def pinned_image:
		    type == "string" and test("^[^@\\s]+@sha256:[0-9a-f]{64}$");
		  to_entries[] as $job |
		  (if ($job.value | has("container")) then
		    $job.value.container as $container |
		    if ($container | type) == "string" then
		      if ($container | pinned_image) then empty
		      else [($job.key + ".container"), ($container | tojson)] end
		    elif ($container | type) == "object" then
		      if ($container | has("image") | not) then
		        [($job.key + ".container.image"), "<missing>"]
		      elif ($container.image | pinned_image) then empty
		      else [($job.key + ".container.image"), ($container.image | tojson)] end
		    else [($job.key + ".container"), ($container | tojson)] end
		  else empty end),
		  (($job.value.services // {}) | to_entries[] as $service |
		    $service.value as $definition |
		    if ($definition | type) != "object" then
		      [($job.key + ".services." + $service.key), ($definition | tojson)]
		    elif ($definition | has("image") | not) then
		      [($job.key + ".services." + $service.key + ".image"), "<missing>"]
		    elif ($definition.image | pinned_image) then empty
		    else
		      [($job.key + ".services." + $service.key + ".image"), ($definition.image | tojson)]
		    end) |
		  @tsv
		'
	)"
	while IFS=$'\t' read -r field image; do
		[[ -n "${field}" ]] || continue
		echo "error: ${workflow#"${root_dir}/"}: ${field} needs an image pinned by sha256 digest; got ${image}" >&2
		failed=true
	done <<<"${container_rows}"

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

	# Named steps keep hosted check failures and release evidence understandable. Use the
	# stable job/index location so malformed or absent names remain actionable.
	step_name_rows="$(
		yq -o=json '.jobs' "${workflow}" | jq -r '
		  to_entries[] as $job |
		  ($job.value.steps // [] | to_entries[]) as $step |
		  $step.value as $value |
		  select(
		    ($value | type) != "object" or
		    ($value | has("name") | not) or
		    ($value.name | type) != "string" or
		    ($value.name | length) == 0
		  ) |
		  [
		    ($job.key + ".steps[" + ($step.key | tostring) + "]"),
		    (if ($value | type) != "object" then ($value | tojson)
		    elif ($value | has("name")) then ($value.name | tojson)
		    else "<missing>" end)
		  ] |
		  @tsv
		'
	)"
	while IFS=$'\t' read -r step name; do
		[[ -n "${step}" ]] || continue
		echo "error: ${workflow#"${root_dir}/"}: step ${step} needs a non-empty string name; got ${name}" >&2
		failed=true
	done <<<"${step_name_rows}"

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

	# Keep every runner on the deliberately selected image. Typed traversal catches quoted
	# latest labels, expressions, self-hosted arrays, and missing fields that text search misses.
	runner_rows="$(
		yq -o=json '.jobs' "${workflow}" | jq -r '
		  to_entries[] as $job |
		  ($job.value["runs-on"]) as $runner |
		  select(($runner | type) != "string" or $runner != "ubuntu-24.04") |
		  [
		    $job.key,
		    (if $job.value | has("runs-on") then ($runner | tojson) else "<missing>" end)
		  ] |
		  @tsv
		'
	)"
	while IFS=$'\t' read -r job runner; do
		[[ -n "${job}" ]] || continue
		echo "error: ${workflow#"${root_dir}/"}: job ${job} needs runs-on: ubuntu-24.04; got ${runner}" >&2
		failed=true
	done <<<"${runner_rows}"
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
echo "GitHub Actions pinning, container-digest, permission-map, checkout-hardening, named-step, bounded-runtime, pinned-runner, concurrency, and serialized-install contracts passed (${#workflows[@]} workflows)"
