#!/usr/bin/env bash
# Exercise agent:new in an isolated source tree; no cluster, model, or network is used.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-agent-new-test.XXXXXX")"
trap 'rm -rf "${tmp_dir}"' EXIT

fail() {
  echo "agent:new fixture failed: $*" >&2
  exit 1
}

mkdir -p \
  "${tmp_dir}/infra" \
  "${tmp_dir}/apps/matrix-a2a-bridge" \
  "${tmp_dir}/scripts" \
  "${tmp_dir}/evals"
cp -R "${repo_root}/infra/kagent" "${tmp_dir}/infra/kagent"
cp -R "${repo_root}/infra/flux" "${tmp_dir}/infra/flux"
cp -R "${repo_root}/apps/matrix-a2a-bridge/deploy" "${tmp_dir}/apps/matrix-a2a-bridge/deploy"
cp -R "${repo_root}/apps/matrix-a2a-bridge/chart" "${tmp_dir}/apps/matrix-a2a-bridge/chart"
mkdir -p \
  "${tmp_dir}/apps/matrix-a2a-bridge/cmd" \
  "${tmp_dir}/apps/matrix-a2a-bridge/internal"
cp "${repo_root}/apps/matrix-a2a-bridge/go.mod" "${tmp_dir}/apps/matrix-a2a-bridge/go.mod"
cp "${repo_root}/apps/matrix-a2a-bridge/go.sum" "${tmp_dir}/apps/matrix-a2a-bridge/go.sum"
cp "${repo_root}/apps/matrix-a2a-bridge/agents.schema.json" "${tmp_dir}/apps/matrix-a2a-bridge/agents.schema.json"
cp -R \
  "${repo_root}/apps/matrix-a2a-bridge/cmd/validate-agents" \
  "${tmp_dir}/apps/matrix-a2a-bridge/cmd/validate-agents"
cp -R \
  "${repo_root}/apps/matrix-a2a-bridge/internal/agentschema" \
  "${tmp_dir}/apps/matrix-a2a-bridge/internal/agentschema"
cp -R "${repo_root}/scripts/testdata" "${tmp_dir}/scripts/testdata"

FGENTIC_REPO_ROOT="${tmp_dir}" mise run agent:new demo-helper >/dev/null

agent_manifest="${tmp_dir}/infra/kagent/agents/demo-helper/agent.yaml"
mapping_component="${tmp_dir}/apps/matrix-a2a-bridge/deploy/agents/demo-helper/kustomization.yaml"
golden_fixture="${tmp_dir}/evals/demo-helper/golden.json"

yq -e '
  (.metadata.name == "demo-helper") and
  (.metadata.namespace == "kagent") and
  (.spec.declarative.modelConfig == "agentgateway-model") and
  (.spec.declarative.deployment.serviceAccountName == "agent-zoo-runtime") and
  ((.spec.declarative.a2aConfig.skills | length) == 1) and
  (((.spec.declarative.tools // []) | length) == 0)
' "${agent_manifest}" >/dev/null || fail "generated Agent contract is invalid"

yq -e '
  .patches[0].patch | from_yaml |
  .[0].value as $mapping |
  (
    ($mapping.namespace == "kagent") and
    ($mapping.name == "demo-helper") and
    ($mapping.stage == "dev") and
    (($mapping.allowedSenders | length) == 1)
  )
' "${mapping_component}" >/dev/null || fail "generated bridge mapping is invalid"
mapping_sender="$(yq -er '.patches[0].patch | from_yaml | .[0].value.allowedSenders[0]' "${mapping_component}")"
[[ "${mapping_sender}" == '@alice:${server_name}' ]] \
  || fail "generated bridge mapping does not use the server-name substitution"

jq -e '
  .schema_version == "fgentic.agent.eval.v1" and
  .agent == "demo-helper" and
  (.agent_contract_sha256 | test("^[0-9a-f]{64}$")) and
  .scenarios[0].agent == "demo-helper" and
  .scenarios[0].rubric.kind == "exact" and
  (.scenarios[0].rubric.expected | length) == 1
' "${golden_fixture}" >/dev/null || fail "generated golden fixture is invalid"

kubectl kustomize "${tmp_dir}/infra/kagent" >"${tmp_dir}/kagent.yaml"
kubectl kustomize "${tmp_dir}/apps/matrix-a2a-bridge/deploy" >"${tmp_dir}/bridge-release.yaml"

yq eval-all -e '
  select(.kind == "Agent" and .metadata.name == "demo-helper") |
  .spec.declarative.a2aConfig.skills | length == 1
' "${tmp_dir}/kagent.yaml" >/dev/null || fail "generated Agent is absent from the effective kagent render"

export server_name=ci.fgentic.example
yq eval-all -o=yaml \
  'select(.kind == "HelmRelease" and .metadata.name == "matrix-a2a-bridge") | .spec.values' \
  "${tmp_dir}/bridge-release.yaml" \
  | flux envsubst --strict \
  | helm template matrix-a2a-bridge "${tmp_dir}/apps/matrix-a2a-bridge/chart" --values - \
    >"${tmp_dir}/bridge.yaml"
yq eval-all -e '
  select(.kind == "ConfigMap" and .metadata.name == "matrix-a2a-bridge-agents") |
  .data."agents.yaml" | from_yaml | .agents."agent-demo-helper" as $mapping |
  (
    ($mapping.namespace == "kagent") and
    ($mapping.name == "demo-helper") and
    ($mapping.stage == "dev") and
    (($mapping.allowedSenders | length) == 1) and
    ($mapping.allowedSenders[0] == "@alice:ci.fgentic.example")
  )
' "${tmp_dir}/bridge.yaml" >/dev/null || fail "generated mapping is absent from effective agents.yaml"

if ! (
  cd "${tmp_dir}"
  bash "${repo_root}/scripts/test-agent-zoo.sh"
) >"${tmp_dir}/agent-zoo.log" 2>&1; then
  cat "${tmp_dir}/agent-zoo.log" >&2
  fail "generated scaffold does not pass the exact check:agents contract"
fi

# Remote targets are governed mappings but deliberately have no local kagent Agent. Prove the
# generalized gate preserves that supported trust boundary while applying sender/stage policy.
bridge_release="${tmp_dir}/apps/matrix-a2a-bridge/deploy/helmrelease.yaml"
cp "${bridge_release}" "${tmp_dir}/bridge-helmrelease.yaml"
yq -i '.spec.values.agents."agent-remote-test" = {
  "url": "https://agents.partner.example/a2a/reviewer",
  "timeout": "30s",
  "tokenBudget": 4096,
  "cardIdentity": {
    "name": "Partner reviewer",
    "organization": "Partner Example",
    "keyID": "partner-reviewer-2026-07",
    "publicKey": {
      "kty": "EC",
      "crv": "P-256",
      "x": "axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY",
      "y": "T-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU"
    }
  },
  "description": "Reviews documents through a governed partner endpoint.",
  "stage": "prod",
  "allowedSenders": ["@alice:${server_name}"]
}' "${bridge_release}"
if ! (
  cd "${tmp_dir}"
  bash "${repo_root}/scripts/test-agent-zoo.sh"
) >"${tmp_dir}/remote-mapping.log" 2>&1; then
  cat "${tmp_dir}/remote-mapping.log" >&2
  fail "valid pinned remote mapping did not pass the generalized check:agents contract"
fi
cp "${tmp_dir}/bridge-helmrelease.yaml" "${bridge_release}"

expect_authoring_failure() {
  local case_name="$1"
  local expected="$2"
  if (
    cd "${tmp_dir}"
    bash "${repo_root}/scripts/test-agent-zoo.sh"
  ) >"${tmp_dir}/${case_name}.log" 2>&1; then
    fail "${case_name} authoring drift unexpectedly passed"
  fi
  rg -q "${expected}" "${tmp_dir}/${case_name}.log" \
    || {
      cat "${tmp_dir}/${case_name}.log" >&2
      fail "${case_name} drift did not produce the actionable validation error"
    }
}

cp "${mapping_component}" "${tmp_dir}/mapping-component.yaml"
yq -i '.patches[0].patch = ((.patches[0].patch | from_yaml) | .[0].value.name = "missing-agent" | to_yaml)' \
  "${mapping_component}"
expect_authoring_failure orphan-mapping 'must resolve to the matching kagent Agent'
cp "${tmp_dir}/mapping-component.yaml" "${mapping_component}"

yq -i '.patches[0].patch = ((.patches[0].patch | from_yaml) | .[0].value.allowedSenders = ["@*:${server_name}"] | to_yaml)' \
  "${mapping_component}"
expect_authoring_failure wildcard-sender 'wildcards and widened allowlists are forbidden'
cp "${tmp_dir}/mapping-component.yaml" "${mapping_component}"

yq -i '.patches[0].patch = ((.patches[0].patch | from_yaml) | .[0].value.allowedSenders += ["@bob:${server_name}"] | to_yaml)' \
  "${mapping_component}"
expect_authoring_failure widened-allowlist 'wildcards and widened allowlists are forbidden'
cp "${tmp_dir}/mapping-component.yaml" "${mapping_component}"

yq -i '.patches[0].patch = ((.patches[0].patch | from_yaml) | .[0].value.stage = "canary" | to_yaml)' \
  "${mapping_component}"
expect_authoring_failure invalid-stage 'agent mapping validation failed'
cp "${tmp_dir}/mapping-component.yaml" "${mapping_component}"

cp "${agent_manifest}" "${tmp_dir}/agent.yaml"
yq -i '.spec.declarative.a2aConfig.skills = []' "${agent_manifest}"
expect_authoring_failure missing-skill 'must advertise at least one A2A skill'
cp "${tmp_dir}/agent.yaml" "${agent_manifest}"

yq -i 'del(.spec.declarative.deployment.serviceAccountName)' "${agent_manifest}"
expect_authoring_failure missing-service-account 'must use the unprivileged shared runtime ServiceAccount'
cp "${tmp_dir}/agent.yaml" "${agent_manifest}"

if FGENTIC_REPO_ROOT="${tmp_dir}" bash "${repo_root}/scripts/new-agent.sh" demo-helper \
  >"${tmp_dir}/duplicate.out" 2>"${tmp_dir}/duplicate.err"; then
  fail "duplicate generation unexpectedly succeeded"
fi
rg -q 'refusing to overwrite existing path' "${tmp_dir}/duplicate.err" \
  || fail "duplicate generation did not fail with an actionable message"

for invalid_name in Demo_Helper ../escape agent/name; do
  if FGENTIC_REPO_ROOT="${tmp_dir}" bash "${repo_root}/scripts/new-agent.sh" "${invalid_name}" \
    >"${tmp_dir}/invalid.out" 2>"${tmp_dir}/invalid.err"; then
    fail "invalid Agent name ${invalid_name} unexpectedly succeeded"
  fi
  rg -q 'lowercase Kubernetes DNS label' "${tmp_dir}/invalid.err" \
    || fail "invalid Agent name ${invalid_name} did not produce the validation error"
done

if rg -n -i '(api[_-]?key|client[_-]?secret|password):[[:space:]]+[^$]' \
  "${agent_manifest}" "${mapping_component}" "${golden_fixture}"; then
  fail "generated files contain credential-shaped data"
fi

echo "Agent scaffold generation and effective render contracts passed."
