#!/usr/bin/env bash
# Runtime GoToSocial/Mastodon-wire interop acceptance for the ActivityPub agent gateway on the DEMO
# profile (issue #489, Task 7). Run AFTER `mise run demo:up`. It proves, over the real HTTP-signature
# transport and the real Postgres dedup ledger:
#
#   AC3  a pinned, allowlisted peer follows the agent BY ITS FEDIVERSE HANDLE, @mentions it, and gets
#        exactly ONE governed, FEP-8b32-signed, budget-admitted A2A-backed reply
#        (apgateway_delegations_total{outcome="ok"} +1, apgateway_budget_reservations_total{outcome="reserved"} +1).
#   FAIL a pinned-but-NON-allowlisted peer is refused fail-closed: 403 +
#        apgateway_rejected_total{reason="border_off_allowlist"} +>=1, with ZERO delegation/reservation deltas.
#   AC4  a byte-identical, re-signed redelivery yields exactly ONE more delegations{outcome="duplicate"}
#        while ok and reserved stay put — no duplicate A2A call, no duplicate reservation (#321).
#
# The peer is a scripted Mastodon/GtS-wire signer (clusters/demo/interop-peer) whose RSA #main-key the
# harness owns, so it can deterministically replay a byte-identical signed activity — which a real
# GoToSocial binary never exposes. The gateway trusts it via an out-of-band pinned key (ADR 0021),
# because an in-cluster peer cannot be SSRF-fetched by the #320-guarded resolver. Every check dies
# loudly on deviation; evidence stays content-free (reasons, counters, HTTP codes — never actor URIs
# or note content).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
# shellcheck source=scripts/lib/activitypub-interop.sh
source "${ROOT_DIR}/scripts/lib/activitypub-interop.sh"

readonly CLUSTER_NAME="${FGENTIC_DEMO_CLUSTER:-fgentic-demo}"
readonly PEER_DEPLOY="activitypub-interop-peer"
readonly PEER_KEYS_SECRET="activitypub-interop-peer-keys"
readonly RUN_ID="$$-${RANDOM}"
readonly ALLOWED_ACTIVITY_ID="${AP_INTEROP_ALLOWED_ACTOR}/activities/mention-${RUN_ID}"
readonly DENIED_ACTIVITY_ID="${AP_INTEROP_DENIED_ACTOR}/activities/mention-${RUN_ID}"

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-ap-interop.XXXXXX")"
readonly WORK_DIR
KUBECONFIG_FILE="${WORK_DIR}/kubeconfig"
PORT_FORWARD_PID=""
POLICY_SUSPENDED="no"
readonly METRICS_LOCAL_PORT=19090

cleanup() {
	[ -n "${PORT_FORWARD_PID}" ] && kill "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true
	if [ -f "${KUBECONFIG_FILE}" ]; then
		export KUBECONFIG="${KUBECONFIG_FILE}"
		kubectl delete namespace "${AP_INTEROP_NAMESPACE}" --wait=false >/dev/null 2>&1 || true
		# Restore the reconciled policy baseline (empty allowlist) and resume Flux.
		if [ "${POLICY_SUSPENDED}" = "yes" ]; then
			restore_policy || true
			flux resume kustomization activitypub >/dev/null 2>&1 || true
		fi
	fi
	rm -rf "${WORK_DIR}"
}
trap cleanup EXIT INT TERM

for command in kubectl k3d flux jq openssl curl; do
	require_command "${command}"
done

# --- Reach the demo cluster on a private kubeconfig (never the user's default context) -------------
k3d kubeconfig get "${CLUSTER_NAME}" >"${KUBECONFIG_FILE}" 2>/dev/null \
	|| die "demo cluster ${CLUSTER_NAME} not found — run 'mise run demo:up' first"
export KUBECONFIG="${KUBECONFIG_FILE}"

GATEWAY_POD=""
resolve_gateway_pod() {
	GATEWAY_POD="$(kubectl --namespace activitypub get pod \
		--selector app.kubernetes.io/name=activitypub-agent-gateway \
		--output jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
	[ -n "${GATEWAY_POD}" ] || die "the ActivityPub gateway is not deployed — did demo:up reconcile it?"
}
resolve_gateway_pod
kubectl --namespace activitypub rollout status "deployment/activitypub-agent-gateway" \
	--timeout=3m >/dev/null || die "the ActivityPub gateway did not become ready"

# --- Metrics scraping over a port-forward (bypasses the metrics-port NetworkPolicy) ---------------
kubectl --namespace activitypub port-forward "pod/${GATEWAY_POD}" \
	"${METRICS_LOCAL_PORT}:9090" >/dev/null 2>&1 &
PORT_FORWARD_PID=$!
for _ in $(seq 1 20); do
	curl --silent --fail "http://127.0.0.1:${METRICS_LOCAL_PORT}/metrics" >/dev/null 2>&1 && break
	sleep 0.5
done

snapshot() { # snapshot <file>
	curl --silent --fail "http://127.0.0.1:${METRICS_LOCAL_PORT}/metrics" >"$1" \
		|| die "could not scrape gateway metrics"
}
mval() { # mval <snapshot-file> <line-regex>  -> counter value (0 when absent)
	local value
	value="$(grep -E "$2" "$1" | awk '{print $NF}' | head -1)"
	echo "${value:-0}"
}
delta_is() { # delta_is <before> <after> <want> <label>
	local got eq
	got="$(awk "BEGIN{print ($2)-($1)}")"
	eq="$(awk "BEGIN{print (${got}==($3))}")"
	[ "${eq}" = "1" ] || die "$4: expected delta $3, observed ${got} (before=$1 after=$2)"
}
gt() { # gt <a> <b>  -> succeeds when a > b
	local res
	res="$(awk "BEGIN{print (($1)>($2))}")"
	[ "${res}" = "1" ]
}

readonly OK_RE='^apgateway_delegations_total\{ghost="agent-docs-qa",outcome="ok"\} '
readonly DUP_RE='^apgateway_delegations_total\{ghost="agent-docs-qa",outcome="duplicate"\} '
readonly RESERVED_RE='^apgateway_budget_reservations_total\{ghost="agent-docs-qa",outcome="reserved"\} '
readonly DENY_RE='^apgateway_rejected_total\{reason="border_off_allowlist"\} '

# --- Provision the peer from the cluster-only demo secret path -------------------------------------
allowed_key="$(kubectl --namespace flux-system get secret fgentic-demo-bootstrap \
	--output "jsonpath={.data.${AP_INTEROP_ALLOWED_KEY}}" | base64 --decode)"
denied_key="$(kubectl --namespace flux-system get secret fgentic-demo-bootstrap \
	--output "jsonpath={.data.${AP_INTEROP_DENIED_KEY}}" | base64 --decode)"
[ -n "${allowed_key}" ] && [ -n "${denied_key}" ] \
	|| die "demo bootstrap is missing the interop peer keys — was demo:up run with this branch?"

kubectl apply --kustomize "${ROOT_DIR}/clusters/demo/interop-peer" >/dev/null
kubectl --namespace "${AP_INTEROP_NAMESPACE}" create secret generic "${PEER_KEYS_SECRET}" \
	--from-literal="allowed.pem=${allowed_key}" \
	--from-literal="denied.pem=${denied_key}" \
	--dry-run=client --output=yaml | kubectl apply --filename - >/dev/null
kubectl --namespace "${AP_INTEROP_NAMESPACE}" rollout status "deployment/${PEER_DEPLOY}" \
	--timeout=3m >/dev/null || die "the interop peer did not become ready"

peer() { # peer <args...>  -> stdout of the peer command
	kubectl --namespace "${AP_INTEROP_NAMESPACE}" exec "deployment/${PEER_DEPLOY}" -- \
		python3 /peer/peer.py "$@"
}
peer_status() { grep -E '^STATUS ' | awk '{print $2}' | head -1; }

# --- Admit ONLY the allowlisted peer via a hot-reloaded policy (proves the git-reloadable border) --
flux suspend kustomization activitypub >/dev/null 2>&1 \
	|| die "could not suspend the activitypub Flux Kustomization for the acceptance"
POLICY_SUSPENDED="yes"
patch_policy() { # patch_policy <policy-json>
	local patch
	patch="$(jq --null-input --arg p "$1" '{data: {"policy.json": $p}}')"
	kubectl --namespace activitypub patch configmap fgentic-activitypub-policy \
		--type merge --patch "${patch}" >/dev/null
}
restore_policy() {
	local baseline
	baseline="$(cat "${ROOT_DIR}/apps/activitypub-agent-gateway/component/policy.json")"
	patch_policy "${baseline}"
}
allow_policy="$(jq --null-input --compact-output \
	--arg actor "${AP_INTEROP_ALLOWED_ACTOR}" --arg domain "${AP_INTEROP_ALLOWED_DOMAIN}" \
	'{version: 1, allowed_domains: [], allowed_actors: [$actor],
	  budgets: {reservation_tokens: 100, default_tokens_per_minute: 0,
	            domains: {($domain): 100000}, actors: {}}}')"
patch_policy "${allow_policy}"

# WebFinger discovery of the agent BY HANDLE (peer -> gateway). Proves handle-based discovery.
discovered_actor="$(peer discover | tail -1)"
[ "${discovered_actor}" = "https://${AP_INTEROP_SERVER_NAME}/ap/agents/${AP_INTEROP_GHOST}" ] \
	|| die "WebFinger did not resolve the agent handle to its actor (got: content withheld)"
echo "discovery: agent resolved by fediverse handle" >&2

# The projected ConfigMap + 5s policy poll take effect asynchronously; poll the Follow until the
# allowlisted peer is admitted (202), bounding the wait.
follow_ok="no"
for _ in $(seq 1 60); do
	follow_out="$(peer follow /keys/allowed.pem "${AP_INTEROP_ALLOWED_ACTOR}" || true)"
	follow_status="$(printf '%s\n' "${follow_out}" | peer_status)"
	if [ "${follow_status}" = "202" ]; then
		follow_ok="yes"
		break
	fi
	sleep 3
done
[ "${follow_ok}" = "yes" ] || die "the allowlisted peer could not Follow the agent within the policy-reload window"
echo "follow: allowlisted peer subscribed by handle (202)" >&2

# --- AC3: one governed, signed, budget-admitted reply ---------------------------------------------
snapshot "${WORK_DIR}/s0.txt"
ok0="$(mval "${WORK_DIR}/s0.txt" "${OK_RE}")"
reserved0="$(mval "${WORK_DIR}/s0.txt" "${RESERVED_RE}")"
dup0="$(mval "${WORK_DIR}/s0.txt" "${DUP_RE}")"

mention_out="$(peer mention /keys/allowed.pem "${AP_INTEROP_ALLOWED_ACTOR}" \
	"${ALLOWED_ACTIVITY_ID}" /tmp/allowed-request.json)"
mention_status="$(printf '%s\n' "${mention_out}" | peer_status)"
[ "${mention_status}" = "202" ] || die "the allowlisted mention was not accepted (202)"
status_path="$(printf '%s\n' "${mention_out}" | grep -E '^LOCATION ' | awk '{print $2}' | head -1)"
[ -n "${status_path}" ] || die "the mention 202 carried no status-capability Location"
echo "mention: governed delegation accepted (202)" >&2

reply_ok="no"
for _ in $(seq 1 40); do
	poll_out="$(peer poll "${status_path}")"
	if echo "${poll_out}" | grep -q '^STATUS 200' && echo "${poll_out}" | grep -q '^PROOF'; then
		reply_ok="yes"
		break
	fi
	sleep 2
done
[ "${reply_ok}" = "yes" ] \
	|| die "no FEP-8b32-signed reply was retrievable for the governed mention within the deadline"
echo "reply: one FEP-8b32-signed A2A-backed reply retrieved" >&2

snapshot "${WORK_DIR}/s1.txt"
ok1="$(mval "${WORK_DIR}/s1.txt" "${OK_RE}")"
reserved1="$(mval "${WORK_DIR}/s1.txt" "${RESERVED_RE}")"
delta_is "${ok0}" "${ok1}" 1 "AC3 delegations{outcome=ok}"
delta_is "${reserved0}" "${reserved1}" 1 "AC3 budget_reservations{outcome=reserved}"
echo "AC3 PASS: exactly one signed, budget-admitted reply" >&2

# --- Fail-closed: a pinned but non-allowlisted peer is refused, zero side effects ------------------
deny0="$(mval "${WORK_DIR}/s1.txt" "${DENY_RE}")"
denied_out="$(peer mention /keys/denied.pem "${AP_INTEROP_DENIED_ACTOR}" "${DENIED_ACTIVITY_ID}" || true)"
denied_status="$(printf '%s\n' "${denied_out}" | peer_status)"
[ "${denied_status}" = "403" ] || die "the non-allowlisted peer was NOT refused with 403"
snapshot "${WORK_DIR}/s2.txt"
deny1="$(mval "${WORK_DIR}/s2.txt" "${DENY_RE}")"
gt "${deny1}" "${deny0}" || die "the refusal did not increment rejected{reason=border_off_allowlist}"
ok2="$(mval "${WORK_DIR}/s2.txt" "${OK_RE}")"
reserved2="$(mval "${WORK_DIR}/s2.txt" "${RESERVED_RE}")"
delta_is "${ok1}" "${ok2}" 0 "fail-closed must not delegate"
delta_is "${reserved1}" "${reserved2}" 0 "fail-closed must not reserve"
echo "FAIL-CLOSED PASS: off-allowlist peer refused (403), no A2A call, no reservation" >&2

# --- AC4: byte-identical redelivery -> exactly one duplicate, no new delegation/reservation --------
replay_status="$(peer replay /tmp/allowed-request.json | peer_status)"
[ "${replay_status}" = "202" ] || die "the byte-identical redelivery was not accepted (202)"
sleep 3
snapshot "${WORK_DIR}/s3.txt"
ok3="$(mval "${WORK_DIR}/s3.txt" "${OK_RE}")"
reserved3="$(mval "${WORK_DIR}/s3.txt" "${RESERVED_RE}")"
dup3="$(mval "${WORK_DIR}/s3.txt" "${DUP_RE}")"
delta_is "${ok1}" "${ok3}" 0 "AC4 redelivery must not add a delegation"
delta_is "${reserved1}" "${reserved3}" 0 "AC4 redelivery must not add a reservation"
gt "${dup3}" "${dup0}" || die "AC4 redelivery did not increment delegations{outcome=duplicate}"
echo "AC4 PASS: redelivery deduped — one duplicate, no repeated A2A call or reservation" >&2

echo "activitypub interop acceptance: AC3 (signed budget-admitted reply), fail-closed off-allowlist refusal, and AC4 (redelivery dedup) all proven over the real transport." >&2
