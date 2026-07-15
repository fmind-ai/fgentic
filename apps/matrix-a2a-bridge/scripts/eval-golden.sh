#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
demo_manifest="${repo_root}/infra/models/demo/server.yaml"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-golden-agent.XXXXXX")"
server_pid=""

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
suite="internal/evaluation/testdata/golden-agent-responses.json"
while IFS= read -r golden; do
	scenario_id="$(jq -er '.scenario_id' <<<"${golden}")"
	prompt="$(jq -er '.prompt' <<<"${golden}")"
	payload="$(jq -cn --arg prompt "${prompt}" \
		'{model:"fgentic-demo",messages:[{role:"user",content:$prompt}]}')"
	answer="$(curl --fail --silent --show-error \
		--header 'Content-Type: application/json' \
		--data "${payload}" \
		"http://127.0.0.1:${port}/v1/chat/completions" \
		| jq -er '.choices[0].message.content')"
	jq -cn --arg scenario_id "${scenario_id}" --arg answer "${answer}" \
		'{scenario_id:$scenario_id,answer:$answer}' >>"${workdir}/answers.jsonl"
done < <(jq -ec '.cases[] | {scenario_id, prompt}' "${suite}")
jq -s '{answers:.}' "${workdir}/answers.jsonl" >"${workdir}/answers.json"

go run ./cmd/eval-golden --suite "${suite}" --actual-answer "${workdir}/answers.json"
