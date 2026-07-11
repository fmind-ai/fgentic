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

rules_file="${workdir}/llm-spend.rules.yaml"
test_file="${workdir}/llm-spend.test.yaml"
export rules_file

flux envsubst --strict < "${repo_root}/infra/observability/monitors/cost-alert.yaml" \
  | yq '.spec' > "${rules_file}"
yq '.rule_files = [strenv(rules_file)]' "${repo_root}/scripts/testdata/llm-spend.test.yaml" \
  > "${test_file}"

promtool check rules "${rules_file}"
promtool test rules "${test_file}"
