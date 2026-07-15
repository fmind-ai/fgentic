#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
demo_manifest="${repo_root}/infra/models/demo/server.yaml"
evals_dir="${repo_root}/evals"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-golden-agent.XXXXXX")"
server_pid=""
attribution_user="@golden-eval:fgentic.localhost"

cleanup() {
	if [[ -n "${server_pid}" ]]; then
		kill "${server_pid}" 2>/dev/null || true
		wait "${server_pid}" 2>/dev/null || true
	fi
	rm -rf "${workdir}"
}
trap cleanup EXIT INT TERM

yq -er 'select(.kind == "ConfigMap" and .metadata.name == "demo-llm") | .data."server.py"' \
	"${demo_manifest}" >"${workdir}/server.py"
yq -er 'select(.kind == "ConfigMap" and .metadata.name == "demo-llm") | .data."response.txt"' \
	"${demo_manifest}" >"${workdir}/response.txt"

HOST=127.0.0.1 PORT=0 PORT_FILE="${workdir}/port" SOURCE_DIR="${workdir}" \
	ATTRIBUTION_LOG="${workdir}/attribution.jsonl" \
	python3 -B "${workdir}/server.py" >"${workdir}/server.log" 2>&1 &
server_pid="$!"

for ((attempt = 0; attempt < 50; attempt++)); do
	if [[ -s "${workdir}/port" ]]; then
		break
	fi
	if ! kill -0 "${server_pid}" 2>/dev/null; then
		cat "${workdir}/server.log" >&2
		exit 1
	fi
	sleep 0.1
done
[[ -s "${workdir}/port" ]] || {
	echo "deterministic demo stub did not publish its loopback port" >&2
	exit 1
}

port="$(<"${workdir}/port")"

# The same response without X-User-Id proves the deterministic backend does not authorize on an
# asserted Matrix identity. Golden requests below still carry the header as forwarded attribution.
anonymous_payload='{"model":"fgentic-demo","messages":[{"role":"user","content":"attribution boundary probe"}]}'
anonymous_answer="$(curl --fail --silent --show-error \
	--header 'Content-Type: application/json' \
	--data "${anonymous_payload}" \
	"http://127.0.0.1:${port}/v1/chat/completions" \
	| jq -er '.choices[0].message.content')"
expected_answer="$(<"${workdir}/response.txt")"
[[ "${anonymous_answer}" == "${expected_answer}" ]] || {
	echo "deterministic model unexpectedly authorized on X-User-Id" >&2
	exit 1
}

while IFS= read -r golden; do
	scenario_id="$(jq -er '.id' <<<"${golden}")"
	prompt="$(jq -er '.prompt' <<<"${golden}")"
	payload="$(jq -cn --arg prompt "${prompt}" \
		'{model:"fgentic-demo",messages:[{role:"user",content:$prompt}]}')"
	answer="$(curl --fail --silent --show-error \
		--header 'Content-Type: application/json' \
		--header "X-User-Id: ${attribution_user}" \
		--data "${payload}" \
		"http://127.0.0.1:${port}/v1/chat/completions" \
		| jq -er '.choices[0].message.content')"
	jq -cn --arg scenario_id "${scenario_id}" --arg answer "${answer}" \
		'{scenario_id:$scenario_id,answer:$answer}' >>"${workdir}/answers.jsonl"
done < <(jq -ec '.scenarios[] | {id, prompt}' "${evals_dir}"/*/golden.json)
jq -s '{answers:.}' "${workdir}/answers.jsonl" >"${workdir}/answers.json"

kustomize build "${repo_root}/infra/kagent" >"${workdir}/agents.yaml"
go run ./cmd/eval-golden \
	--evals "${evals_dir}" \
	--agents "${workdir}/agents.yaml" \
	--prompts "${workdir}/agents.yaml" \
	--actual-answer "${workdir}/answers.json"

jq -s -e --arg user_id "${attribution_user}" \
	'.[0].user_id == null and (.[1:] | length > 0) and all(.[1:][]; .user_id == $user_id)' \
	"${workdir}/attribution.jsonl" >/dev/null || {
	echo "golden runner did not preserve the X-User-Id attribution-only boundary" >&2
	exit 1
}
echo "X-User-Id attribution-only boundary passed: header forwarded, anonymous request equally admitted."
