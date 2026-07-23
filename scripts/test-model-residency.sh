#!/usr/bin/env bash
# Prove #339 classification/residency-aware model routing, fail-closed, entirely offline (the
# optional --runtime block additionally evaluates the real agentgateway CEL in Docker). The gate
# has three offline layers:
#   1. Manifest contract: the agentgateway A2A authorization policy ANDs a second Require rule that
#      compares the bridge-forwarded classification rank to the selected model's ceiling rank, with
#      fail-closed fallthroughs on BOTH sides (missing header -> regulated; unknown ceiling ->
#      public), on the kagent egress chokepoint (D11 — enforced at the gateway, not trusted kagent).
#   2. Backstop contract: the sovereign (vLLM) provider egress NetworkPolicy exposes NO external
#      egress path, and the model namespace is default-deny egress — the load-bearing backstop.
#   3. Decision matrix: the authoritative governed evaluator (check-model-catalog --admit) decides
#      allow/deny for fixture requests, proving a classified room is served only by the sovereign
#      backend, a hyperscaler target for that class is denied, and a missing/unknown class fails
#      closed to the most-restrictive class. Flipping any sovereign fixture to a hyperscaler here
#      flips ALLOW->DENY and fails the gate, so the deny path is real, not asserted.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
policy_file="${repo_root}/infra/agentgateway/a2a-authorization.yaml"
vllm_egress_file="${repo_root}/infra/agentgateway/providers/profiles/vllm/networkpolicy.yaml"
# Fixture workload key for the --runtime Docker gateway only (never a real credential); held in a
# variable so the literal is not inline in a curl Authorization header (gitleaks curl-auth-header).
fixture_bridge_key="fixture-bridge-key"
models_np_file="${repo_root}/infra/models/vllm/networkpolicy.yaml"
bridge_dir="${repo_root}/apps/matrix-a2a-bridge"
runtime=false

if [ "${1:-}" = "--runtime" ]; then
	runtime=true
elif [ "$#" -ne 0 ]; then
	echo "usage: $0 [--runtime]" >&2
	exit 2
fi

fail() {
	echo "error: $*" >&2
	exit 1
}

assert_equal() {
	[ "$1" = "$2" ] || fail "$3: expected '$2', got '$1'"
}

assert_contains() {
	[[ "$1" == *"$2"* ]] || fail "$3: missing '$2'"
}

refute_contains() {
	[[ "$1" != *"$2"* ]] || fail "$3: unexpected '$2'"
}

# ---------------------------------------------------------------------------
# Layer 1: agentgateway residency authorization contract (offline)
# ---------------------------------------------------------------------------
auth_action="$(yq -r '
  select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization")
  | .spec.traffic.authorization.action
' "${policy_file}")"
assert_equal "${auth_action}" "Require" "residency authorization action is fail-closed Require"

# ONE &&-joined Require expression (matching every other policy in the repo), so there is no
# AND-vs-OR ambiguity across multiple list elements and the CEL evaluation in Layer 3 covers the
# whole admission decision, not the residency rule alone.
expr_count="$(yq -r '
  select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization")
  | .spec.traffic.authorization.policy.matchExpressions | length
' "${policy_file}")"
assert_equal "${expr_count}" "1" "authorization is a single &&-joined expression (no multi-element combining)"

policy_expr="$(yq -r '
  select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization")
  | .spec.traffic.authorization.policy.matchExpressions[0]
' "${policy_file}")"
# The single expression must fold the workload rule AND the residency rule together.
assert_contains "${policy_expr}" 'apiKey.workload == "matrix-a2a-bridge"' "workload rule folded into the single expression"
assert_contains "${policy_expr}" 'request.headers["x-fgentic-data-classification"]' "residency rule folded into the single expression"
assert_contains "${policy_expr}" 'model_allowed_classification' "ceiling is substituted from the governed platform-settings value"
echo "residency authorization contract passed"

# ---------------------------------------------------------------------------
# Layer 2: sovereign backstop NetworkPolicy contract (offline)
# ---------------------------------------------------------------------------
vllm_egress="$(yq -r '
  select(.kind == "NetworkPolicy" and .metadata.name == "agentgateway-vllm-egress")
' "${vllm_egress_file}")"
assert_contains "${vllm_egress}" "Egress" "sovereign profile constrains proxy egress"
# The load-bearing backstop: while the sovereign profile is selected the proxy has NO ipBlock/CIDR
# egress rule, so there is no reachable external-provider path even if the CEL were bypassed.
refute_contains "${vllm_egress}" "ipBlock" "sovereign proxy egress must have no external CIDR path"
refute_contains "${vllm_egress}" "0.0.0.0/0" "sovereign proxy egress must not reach the public internet"

models_deny="$(yq -r '
  select(.kind == "NetworkPolicy" and .metadata.name == "models-default-deny")
  | .spec.policyTypes | join(",")
' "${models_np_file}")"
assert_contains "${models_deny}" "Egress" "model namespace is default-deny egress"
echo "sovereign backstop NetworkPolicy contract passed"

# ---------------------------------------------------------------------------
# Layer 3: evaluate the ACTUAL shipped CEL against the fixture matrix (authoritative)
# ---------------------------------------------------------------------------
# This compiles the exact matchExpression extracted from the policy YAML with cel-go and evaluates
# it against fixture request contexts (workload + path + method + classification header) with the
# ceiling substituted as Flux would. It is NOT a substring check or a Go re-implementation, so an
# inverted/weakened comparison that admits classified content to a hyperscaler makes this FAIL.
# modelcatalog.Admits is retained inside the harness only as a governed-model cross-check.
(cd "${bridge_dir}" && go run ./cmd/check-model-residency --repo-root ../..)

echo "model residency contract passed"

if ! ${runtime}; then
	exit 0
fi

# ---------------------------------------------------------------------------
# Optional runtime: evaluate the REAL agentgateway CEL against fixture requests
# ---------------------------------------------------------------------------
command -v docker >/dev/null 2>&1 || fail "docker is required for --runtime"
docker info >/dev/null 2>&1 || fail "Docker daemon is not running"
command -v curl >/dev/null 2>&1 || fail "curl is required for --runtime"

agentgateway_version="$(yq -r '
  select(.kind == "OCIRepository" and .metadata.name == "agentgateway")
  | .spec.ref.tag
' "${repo_root}/infra/flux/sources.yaml")"
[ -n "${agentgateway_version}" ] && [ "${agentgateway_version}" != "null" ] || fail "agentgateway version pin is missing"
agentgateway_image="${AGENTGATEWAY_IMAGE:-cr.agentgateway.dev/agentgateway:${agentgateway_version}}"

workdir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-model-residency.XXXXXX")"
container="fgentic-model-residency-${RANDOM}-$$"
cleanup() {
	docker rm -f "${container}" >/dev/null 2>&1 || true
	rm -rf "${workdir}"
}
trap cleanup EXIT

# Render the full folded expression with a concrete ceiling, exactly as Flux substitution would.
# The expression includes the workload rule, so the config below also configures strict API-key
# auth and every request carries the bridge bearer — proving the combined workload && residency
# policy on the real gateway.
render_rule() {
	echo "${policy_expr//\$\{model_allowed_classification\}/$1}"
}

run_gateway() {
	local ceiling="$1" rendered_rule
	rendered_rule="$(render_rule "${ceiling}")"
	docker rm -f "${container}" >/dev/null 2>&1 || true
	cat >"${workdir}/config.yaml" <<EOF
binds:
- port: 3000
  listeners:
  - routes:
    - matches:
      - path:
          pathPrefix: /api/a2a
      policies:
        apiKey:
          mode: strict
          keys:
          - key: ${fixture_bridge_key}
            metadata:
              workload: matrix-a2a-bridge
        authorization:
          rules:
          - >-
            ${rendered_rule}
        directResponse:
          body: authorized
          status: 200
EOF
	docker run --rm --name "${container}" \
		-p 127.0.0.1::3000 \
		-v "${workdir}/config.yaml:/config.yaml:ro" \
		-d "${agentgateway_image}" -f /config.yaml >/dev/null
	host_port=""
	for _ in {1..50}; do
		host_port="$(docker port "${container}" 3000/tcp 2>/dev/null | sed -n 's/.*:\([0-9][0-9]*\)$/\1/p' | head -1)"
		if [ -n "${host_port}" ] && curl --silent --output /dev/null --header "Authorization: Bearer ${fixture_bridge_key}" "http://127.0.0.1:${host_port}/api/a2a/kagent/probe"; then
			break
		fi
		sleep 0.2
	done
	[ -n "${host_port}" ] || fail "agentgateway did not publish its test port"
}

request_status() {
	# request_status <classification-or-empty>
	local args=(--silent --output /dev/null --write-out '%{http_code}' --request POST --header "Authorization: Bearer ${fixture_bridge_key}")
	if [ -n "$1" ]; then
		args+=(--header "X-Fgentic-Data-Classification: $1")
	fi
	curl "${args[@]}" "http://127.0.0.1:${host_port}/api/a2a/kagent/probe"
}

assert_status() {
	# assert_status <classification-or-empty> <expected-code> <label> — captures the request in a
	# standalone assignment so the command substitution's return is not masked (SC2312).
	local got
	got="$(request_status "$1")"
	assert_equal "${got}" "$2" "$3"
}

# Public ceiling (hyperscaler profile): only public content is admitted; classified fails closed.
run_gateway public
assert_status public "200" "public content admitted under public ceiling"
assert_status regulated "403" "regulated content denied under public ceiling"
assert_status restricted "403" "restricted content denied under public ceiling"
assert_status confidential "403" "unknown class denied under public ceiling"
assert_status '' "403" "missing header fails closed under public ceiling"

# Regulated ceiling (sovereign profile): every class up to regulated is admitted.
run_gateway regulated
assert_status regulated "200" "regulated content admitted under regulated ceiling"
assert_status public "200" "public content admitted under regulated ceiling"
assert_status '' "200" "missing header (regulated default) admitted under regulated ceiling"

echo "model residency runtime contract passed (${agentgateway_image})"
