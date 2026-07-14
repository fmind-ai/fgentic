#!/usr/bin/env bash
# Extract the PrometheusRule payload after Flux substitution, then validate and unit-test the
# exact rules Prometheus Operator will load. Keeping extraction here avoids duplicating rules in
# a test-only file that could drift from the Kubernetes resource.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

export llm_usage_budget_15m
llm_usage_budget_15m="$(yq -r '.data.llm_usage_budget_15m' "${repo_root}/clusters/gcp/platform-settings.yaml")"

run_rule_test() {
	local manifest="$1"
	local fixture="$2"
	local stem="$3"
	local rules_file="${workdir}/${stem}.rules.yaml"
	local test_file="${workdir}/${stem}.test.yaml"

	flux envsubst --strict < "${manifest}" \
		| yq 'select(.kind == "PrometheusRule") | .spec' > "${rules_file}"
	RULES_FILE="${rules_file}" yq '.rule_files = [strenv(RULES_FILE)]' "${fixture}" > "${test_file}"

	promtool check rules "${rules_file}"
	promtool test rules "${test_file}"
}

run_rule_test \
	"${repo_root}/infra/observability/monitors/cost-alert.yaml" \
	"${repo_root}/scripts/testdata/llm-spend.test.yaml" \
	"llm-spend"
run_rule_test \
	"${repo_root}/infra/observability/monitors/trivy-alert.yaml" \
	"${repo_root}/scripts/testdata/trivy-vulnerability-alert.test.yaml" \
	"trivy-vulnerability-alert"
