#!/usr/bin/env bash
# Definition-only federation acceptance contracts sourced by scripts/test-federation.sh.
check_federation_acceptance() {
local a2a_seed="${ROOT_DIR}/scripts/lib/federation-a2a.sh"
local quota_line receipt_verify_line
# The up path proves both boundaries: the hardened v12 room still exchanges A/B messages and
# rejects C, while a final custom event in a throwaway room is retained by B but dropped by A with
# a content-free, event-addressable policy record. The explicit allow mode is used only by the
# ephemeral-Git reload drill; deny remains the canonical default.
for contract in \
		'FGENTIC_DEMO_PROFILE=federation' \
		'A2A_URL="https://a2a.${SERVER_A}"' \
		'IDP_B_URL="https://id.${SERVER_B}"' \
		'verify_public_agent_card' \
		'public AgentCard bytes differ from the signed bootstrap artifact' \
		'.securitySchemes.orgBOIDC.openIdConnectSecurityScheme.openIdConnectUrl' \
		'org-B OIDC discovery contract is inconsistent' \
		'verify_kagent_not_public' \
		'(.spec.type // "ClusterIP") == "ClusterIP"' \
		'a Gateway API route exposes kagent directly' \
		'reset_delegation_quota_fixture' \
		'redis-cli FLUSHDB' \
		'production quotas must' \
		'client_credentials_token org-b-a2a' \
		'client_credentials_token untrusted-a2a' \
		'client_credentials_token wrong-audience-a2a' \
		'expect_a2a_status missing-token 401' \
		'expect_a2a_status malformed-token 401' \
		'expect_a2a_status wrong-audience 401' \
		'expect_a2a_status untrusted-consumer 403' \
		'A2A-Version: 1.0' \
		'A2A-Extensions: ${TOKEN_BUDGET_EXTENSION}' \
		'missing A2A extension activation returned HTTP' \
		'expect_a2a_status unsupported-method 403' \
		'invalid-budget-' \
		'unpublished kagent path returned HTTP' \
		'agentgateway_token_total' \
		'authorized org B delegation returned HTTP' \
		'authorized delegation did not increase aggregate model-token metrics' \
		'expect_a2a_status exhausted-reservation-quota 429' \
		'The limiter reserves the caller-declared maximum' \
		'room_version' \
	'"12"' \
	'creation_content: {"m.federate": true}' \
	'creation_content: {"m.federate": false}' \
	'm.room.server_acl' \
	'allow_ip_literals: false' \
	'.allow_ip_literals == false and .deny == []' \
	'(.allow | sort) == ([$a, $b] | sort)' \
	'/state/m.room.server_acl' \
	'.room_version == "12" and ."m.federate" == true' \
	'.room_version == "12" and ."m.federate" == false' \
	'/_matrix/client/v3/rooms/' \
	'/invite' \
	'/join' \
	'/send/m.room.message/' \
	'/sync?timeout=1000' \
	'@alice:${SERVER_A}' \
	'@bob:${SERVER_B}' \
	'@charlie:${SERVER_C}' \
	'MATRIX_C_URL' \
	'register_user matrix-c' \
	'create_federated_room' \
	'denied control join' \
	'send_signed_federation_probe' \
	'SYNAPSE_SIGNING_KEY' \
	'from signedjson.key import read_signing_keys' \
	'from signedjson.sign import sign_json' \
	'edu_type: "m.typing"' \
	'signed federation positive control' \
	'denied control federation send to' \
	'whitelist | sort) == ([$a, $b, $c] | sort)' \
	'SYNAPSE_REGISTRATION_SHARED_SECRET' \
	'whitelist_enabled == true' \
	'federation-a-to-b-' \
	'federation-b-to-a-' \
	'POLICY_EVENT_TYPE="com.fgentic.blocked"' \
	'POLICY_PROBE_MODE="${FGENTIC_FED_POLICY_PROBE:-deny}"' \
	'FGENTIC_FED_POLICY_PROBE must be allow or deny' \
	'wait_for_mounted_policy_mode matrix' \
	'wait_for_mounted_policy_mode matrix-b' \
	'/etc/fgentic/federation-policy/policy.json' \
	'(.allowed_event_types | index($type)) != null' \
	'(.allowed_event_types | index($type)) == null' \
	'Fgentic Federation Policy Probe' \
	'policy-content-must-not-be-logged-' \
	'verify_local_policy_event' \
	'wait_for_policy_violation' \
	'select(.event == $event_id)' \
	'fgentic_federation_policy_violation ' \
	'event_type_not_allowed' \
	'allowed_event_type_count' \
	'policy_digest' \
	'federation policy logs exposed denied event content' \
	'verify_remote_policy_event_absent' \
	'.errcode == "M_NOT_FOUND"' \
	'wait_for_remote_policy_event' \
	'allowed on A after Flux policy reconcile'; do
	rg --fixed-strings "${contract}" "${LIFECYCLE}" "${SEED_SOURCES[@]}" >/dev/null ||
		fail "federation acceptance proof omits ${contract}"
done
quota_line="$(rg --line-number --fixed-strings \
	'expect_a2a_status exhausted-reservation-quota 429' "${a2a_seed}" | cut -d: -f1)"
receipt_verify_line="$(rg --line-number --fixed-strings \
	'"${ROOT_DIR}/scripts/usage-receipt.sh" verify --input "${receipt}"' \
	"${a2a_seed}" | cut -d: -f1)"
[[ "${quota_line}" =~ ^[1-9][0-9]*$ && "${receipt_verify_line}" =~ ^[1-9][0-9]*$ ]] &&
	((quota_line < receipt_verify_line)) ||
	fail 'quota denial must run before local receipt verification can outlive the minute window'
if rg --fixed-strings '403 | 404' "${SEED_SOURCES[@]}" >/dev/null; then
	fail 'federation acceptance treats a local missing-room response as a denied federation send'
fi
if rg --fixed-strings -- '--data-urlencode "client_secret=' "${SEED_SOURCES[@]}" >/dev/null; then
	fail 'federation acceptance exposes a confidential-client secret in process arguments'
fi
if rg --fixed-strings '%3N' "${SEED_SOURCES[@]}" >/dev/null; then
	fail 'federation acceptance depends on GNU date nanosecond formatting'
fi
rg --fixed-strings 'LLM_PROVIDER="demo"' "${DEMO_SOURCES[@]}" >/dev/null ||
	fail 'federation profile can select a paid model provider'

}
