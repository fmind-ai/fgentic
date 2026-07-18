#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir
readonly workflow="${root_dir}/.github/workflows/smoke.yml"
readonly diagnostics="${root_dir}/scripts/collect-smoke-diagnostics.sh"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-smoke-workflow.XXXXXX")"
readonly work_dir

cleanup() {
	rm -rf "${work_dir}"
}
trap cleanup EXIT INT TERM

bash -n "${diagnostics}"

yq -e '
  .on.schedule[0].cron != null and
  .on.workflow_dispatch.inputs.fault_injection.type == "choice" and
  (.on.workflow_dispatch.inputs.fault_injection.options | contains(["bad-model-image"])) and
  .permissions.contents == "read" and
  .concurrency."cancel-in-progress" == false and
  .jobs.demo."timeout-minutes" < 20 and
  .jobs.policy."timeout-minutes" < 20 and
  .jobs.scanner."timeout-minutes" == 35 and
  .jobs.policy.env.K3D_AUDIT_CLUSTER_NAME == "fgentic-smoke-api-audit" and
  .jobs.scanner.env.TRIVY_OPERATOR_CLUSTER_NAME == "fgentic-smoke-trivy" and
  (.jobs.demo | has("needs") | not) and
  (.jobs.policy | has("needs") | not) and
  (.jobs.scanner | has("needs") | not) and
  ([.jobs.demo.steps[] | select((.uses // "") | test("^actions/cache@[0-9a-f]{40}$"))] |
    length) > 0 and
  ([.jobs.demo.steps[] |
    select((.uses // "") | test("^docker/setup-buildx-action@[0-9a-f]{40}$"))] |
    length) > 0 and
  ([.jobs.demo.steps[] | select(.run == "mise run demo:up")] | length) > 0 and
  ([.jobs.demo.steps[] |
    select(.id == "quota_usage" and ."continue-on-error" == true and
      ((.run // "") | contains("kubectl get resourcequota --all-namespaces")) and
      ((.run // "") | contains("--field-selector metadata.name=compute-budget")) and
      ((.run // "") | contains("agentgateway-system")) and
      ((.run // "") | contains("knowledge")) and
      ((.run // "") | contains("postgres")))] | length) == 1 and
  ([.jobs.demo.steps[] |
    select(.run == "mise run demo:down" and (.if | contains("always")))] | length) > 0 and
  ([.jobs.demo.steps[] |
    select(((.run // "") | contains("infra/models/demo/server.yaml")) and
      ((.env.BAD_MODEL_IMAGE // "") | contains("sha256:0000000000000000")))] | length) > 0 and
  ([.jobs.demo.steps[] |
    select((.run // "") == "bash scripts/collect-smoke-diagnostics.sh demo-up")] | length) > 0 and
  ([.jobs.demo.steps[] | select(.name == "Enforce demo result") |
    select((.run // "") | contains("steps.quota_usage.outcome"))] | length) == 1 and
  ([.jobs.demo.steps[] |
    select(((.uses // "") | test("^actions/upload-artifact@[0-9a-f]{40}$")) and
      (.if | contains("failure")))] |
    length) > 0 and
  ([.jobs.policy.steps[] | select(.run == "mise run test:a2a-authorization")] | length) > 0 and
  ([.jobs.policy.steps[] | select(.run == "mise run test:mcp-governance")] | length) > 0 and
  ([.jobs.policy.steps[] | select(.run == "mise run test:tracing")] | length) > 0 and
  ([.jobs.policy.steps[] | select(.run == "mise run test:network-policies:kind")] | length) > 0 and
  ([.jobs.policy.steps[] |
    select(.run == "mise run test:resource-quotas" and .id == "quota" and
      ."continue-on-error" == true and .env.KIND_CLUSTER_NAME == "fgentic-smoke-quota")] |
    length) == 1 and
  ([.jobs.policy.steps[] | select(.name == "Record policy results") |
    select((.run // "") | contains("quota=${{ steps.quota.outcome }}"))] | length) == 1 and
  ([.jobs.policy.steps[] | select(.name == "Enforce policy result") |
    select((.run // "") | contains("steps.quota.outcome"))] | length) == 1 and
  ([.jobs.policy.steps[] |
    select(.id == "kubernetes_audit" and
      ."continue-on-error" == true and
      .run == "mise run test:kubernetes-audit")] | length) == 1 and
  ([.jobs.policy.steps[] |
    select(((.run // "") | contains("kubernetes_audit=${{ steps.kubernetes_audit.outcome }}")))] |
    length) == 1 and
  ([.jobs.policy.steps[] |
    select(((.run // "") | contains("steps.kubernetes_audit.outcome }}\" != \"success\"")))] |
    length) == 1 and
  ([.jobs.policy.steps[] |
    select(((.uses // "") | test("^actions/upload-artifact@[0-9a-f]{40}$")) and
      (.if | contains("failure")))] |
    length) > 0 and
  ([.jobs.scanner.steps[] |
    select(.id == "trivy_operator" and ."continue-on-error" == true and
      .run == "timeout --signal=TERM --kill-after=90s 30m mise run test:trivy-operator")] |
    length) == 1 and
  ([.jobs.scanner.steps[] | select(.name == "Enforce scanner result") |
    select((.run // "") | contains("steps.trivy_operator.outcome"))] | length) == 1 and
  ([.jobs.scanner.steps[] |
    select(((.uses // "") | test("^actions/upload-artifact@[0-9a-f]{40}$")) and
      (.if | contains("failure")))] |
    length) > 0 and
  .jobs.report.needs[0] == "demo" and .jobs.report.needs[1] == "policy" and
  .jobs.report.needs[2] == "scanner" and
  .jobs.report.if ==
    "always() && github.ref_name == github.event.repository.default_branch" and
  .jobs.report.permissions.issues == "write" and
  ([.jobs.report.steps[] |
    select(((.run // "") | contains("gh issue create")) and
      ((.run // "") | contains("gh issue reopen")) and
      ((.run // "") | contains("gh issue comment")) and
      ((.run // "") | contains("gh issue close")) and
      ((.run // "") | contains("SCANNER_RESULT")))] | length) > 0
' "${workflow}" >/dev/null

rg --fixed-strings 'kubectl --request-timeout=30s describe pods --all-namespaces' \
	"${diagnostics}" >/dev/null
rg --fixed-strings -- '--all-containers=true --prefix=true' "${diagnostics}" >/dev/null

# Execute the exact workflow mutation against a scratch manifest. This proves the deliberate
# break is real without touching the checked-out demo profile or creating a cluster.
mkdir -p "${work_dir}/infra/models/demo"
cp "${root_dir}/infra/models/demo/server.yaml" "${work_dir}/infra/models/demo/server.yaml"
yq -r '.jobs.demo.steps[] | select(.name == "Inject an invalid demo model image") | .run' \
	"${workflow}" >"${work_dir}/inject.sh"
bad_model_image="$(
	yq -r '
    .jobs.demo.steps[] | select(.name == "Inject an invalid demo model image") |
      .env.BAD_MODEL_IMAGE
  ' "${workflow}"
)"
(
	cd "${work_dir}"
	BAD_MODEL_IMAGE="${bad_model_image}" bash inject.sh
)
test "$(
	yq -r '
    select(.kind == "Deployment" and .metadata.name == "demo-llm") |
      .spec.template.spec.containers[] | select(.name == "server") | .image
  ' "${work_dir}/infra/models/demo/server.yaml"
)" = "${bad_model_image}"

# The issue-maintenance shell must remain syntactically valid, but is intentionally never executed
# by this offline check because it has write authority in GitHub Actions.
yq -r '.jobs.report.steps[0].run' "${workflow}" >"${work_dir}/report.sh"
bash -n "${work_dir}/report.sh"

echo "Nightly smoke workflow contract passed"
