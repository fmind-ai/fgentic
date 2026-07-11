#!/usr/bin/env bash
set -euo pipefail

# Port-forwarding is transport setup only. Scenario execution, A2A parsing, metric attribution,
# scoring, pricing, and report merging stay in the typed Go harness.
forward_log="$(mktemp "${TMPDIR:-/tmp}/fgentic-model-eval-port-forward.XXXXXX")"
forward_pid=""

cleanup() {
	if [[ -n "${forward_pid}" ]]; then
		kill "${forward_pid}" 2>/dev/null || true
		wait "${forward_pid}" 2>/dev/null || true
	fi
	rm -f "${forward_log}"
}
trap cleanup EXIT INT TERM

kubectl --namespace agentgateway-system port-forward deployment/agentgateway-proxy \
	18080:8080 15020:15020 >"${forward_log}" 2>&1 &
forward_pid="$!"

for ((attempt = 0; attempt < 50; attempt++)); do
	if ! kill -0 "${forward_pid}" 2>/dev/null; then
		cat "${forward_log}" >&2
		exit 1
	fi
	if curl --fail --silent --show-error http://127.0.0.1:15020/metrics >/dev/null 2>&1; then
		go run ./cmd/eval "$@"
		exit $?
	fi
	sleep 0.1
done

cat "${forward_log}" >&2
echo "agentgateway port-forward did not become ready" >&2
exit 1
