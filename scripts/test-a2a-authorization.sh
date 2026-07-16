#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
policy_file="${repo_root}/infra/agentgateway/a2a-authorization.yaml"
route_file="${repo_root}/infra/agentgateway/a2a-route.yaml"
owner_file="${repo_root}/infra/agentgateway/kustomization.yaml"
secret_example="${repo_root}/infra/secrets/a2a-authorization.sops.yaml.example"
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
	local actual="$1"
	local expected="$2"
	local label="$3"
	[ "${actual}" = "${expected}" ] || fail "${label}: expected '${expected}', got '${actual}'"
}

assert_contains() {
	local actual="$1"
	local expected="$2"
	local label="$3"
	[[ "${actual}" == *"${expected}"* ]] || fail "${label}: missing '${expected}'"
}

expression="$(
	yq -r '
    select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization")
    | .spec.traffic.authorization.policy.matchExpressions[0]
  ' "${policy_file}"
)"

assert_equal "$(yq -r '.spec.traffic.apiKeyAuthentication.mode' "${policy_file}")" "Strict" "API-key mode"
assert_equal "$(yq -r '.spec.traffic.apiKeyAuthentication.secretRef.name' "${policy_file}")" "a2a-bridge-callers" "API-key Secret"
assert_equal "$(yq -r '.spec.traffic.authorization.action' "${policy_file}")" "Require" "authorization action"
for guarded_file in "${route_file}" "${policy_file}"; do
	assert_equal "$({
		yq -r '.metadata.annotations."kustomize.toolkit.fluxcd.io/prune"' "${guarded_file}"
	})" "disabled" "$(basename "${guarded_file}") ownership-handoff prune guard"
done
assert_equal "$({
	yq -N -r '
    [.resources[] | select(. == "a2a-route.yaml" or . == "a2a-authorization.yaml")]
    | length
  ' "${owner_file}"
})" "2" "current A2A admission owner inventory"
assert_contains "${expression}" 'apiKey.workload == "matrix-a2a-bridge"' "workload authorization"
assert_contains "${expression}" 'request.path.startsWith("/api/a2a/kagent/")' "kagent path boundary"
assert_contains "${expression}" 'request.method == "POST"' "A2A method boundary"
assert_contains "${expression}" 'request.method == "GET"' "AgentCard method boundary"
assert_contains "${expression}" 'request.path.endsWith("/.well-known/agent-card.json")' "AgentCard path boundary"

assert_equal "$({
	yq -r '
    select(.kind == "Secret" and .metadata.name == "a2a-bridge-callers")
    | .stringData."matrix-a2a-bridge"
    | from_json
    | .metadata.workload
  ' "${secret_example}"
})" "matrix-a2a-bridge" "credential metadata"

echo "A2A authorization manifest contract passed"

if ! ${runtime}; then
	exit 0
fi

command -v docker >/dev/null 2>&1 || fail "docker is required for --runtime"
docker info >/dev/null 2>&1 || fail "Docker daemon is not running"
command -v curl >/dev/null 2>&1 || fail "curl is required for --runtime"

agentgateway_version="$(
	yq -r '
    select(.kind == "OCIRepository" and .metadata.name == "agentgateway")
    | .spec.ref.tag
  ' "${repo_root}/infra/flux/sources.yaml"
)"
[ -n "${agentgateway_version}" ] && [ "${agentgateway_version}" != "null" ] || fail "agentgateway version pin is missing"
agentgateway_image="${AGENTGATEWAY_IMAGE:-cr.agentgateway.dev/agentgateway:${agentgateway_version}}"

workdir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-a2a-authorization.XXXXXX")"
container="fgentic-a2a-authorization-$RANDOM-$$"
cleanup() {
	docker rm -f "${container}" >/dev/null 2>&1 || true
	rm -rf "${workdir}"
}
trap cleanup EXIT

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
          - key: fixture-bridge-key
            metadata:
              workload: matrix-a2a-bridge
          - key: fixture-other-key
            metadata:
              workload: unrelated-workload
        authorization:
          rules:
          - >-
            ${expression}
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
	if [ -n "${host_port}" ] && curl --silent --output /dev/null "http://127.0.0.1:${host_port}/api/a2a/kagent/platform-helper"; then
		break
	fi
	sleep 0.2
done
[ -n "${host_port}" ] || fail "agentgateway did not publish its test port"

request_status() {
	local method="$1"
	local path="$2"
	local key="${3:-}"
	local args=(--silent --show-error --output "${workdir}/response" --write-out '%{http_code}' --request "${method}")
	if [ -n "${key}" ]; then
		args+=(--header "Authorization: Bearer ${key}")
	fi
	curl "${args[@]}" "http://127.0.0.1:${host_port}${path}"
}

assert_equal "$(request_status POST /api/a2a/kagent/platform-helper)" "401" "missing credential"
assert_equal "$(request_status POST /api/a2a/kagent/platform-helper invalid-key)" "401" "invalid credential"
assert_equal "$(request_status POST /api/a2a/kagent/platform-helper fixture-other-key)" "403" "wrong workload"
assert_equal "$(request_status POST /api/a2a/other/platform-helper fixture-bridge-key)" "403" "wrong A2A namespace"
assert_equal "$(request_status GET /api/a2a/kagent/platform-helper fixture-bridge-key)" "403" "non-AgentCard GET"
assert_equal "$(request_status DELETE /api/a2a/kagent/platform-helper fixture-bridge-key)" "403" "unsupported method"
assert_equal "$(request_status POST /api/a2a/kagent/platform-helper fixture-bridge-key)" "200" "authorized A2A request"
assert_equal "$(request_status GET /api/a2a/kagent/platform-helper/.well-known/agent-card.json fixture-bridge-key)" "200" "authorized AgentCard request"

echo "A2A authorization runtime contract passed (${agentgateway_image})"
