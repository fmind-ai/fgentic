#!/usr/bin/env bash
# Definition-only federation acceptance contracts sourced by scripts/test-federation.sh.
# shellcheck disable=SC2016 # jq bindings and source-contract placeholders are intentionally literal
# shellcheck disable=SC2312 # substitutions feed fail-closed assertions or mandatory fixture execution
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
		'verify_agent_card_rotation' \
		'--additional-key "${overlap_kid}=${overlap_jwk}"' \
		'--revoked-key-id "${revoked_kid}"' \
		'revoked-kid AgentCard was accepted after revocation' \
		'tampered AgentCard was accepted mid-overlap' \
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
		'(.allow | sort) == ([$a, $b, $d] | sort)' \
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
		'@dave:${SERVER_D}' \
		'@charlie:${SERVER_C}' \
		'MATRIX_C_URL' \
		'MATRIX_D_URL' \
		'register_user matrix-c' \
		'register_user matrix-d' \
		'A2A_D_URL="https://a2a-d.${SERVER_A}"' \
		'client_credentials_token org-d-a2a' \
		'verify_org_d_delegation' \
		'authorized org D delegation returned HTTP' \
		'usage_receipt_archive_count_d' \
		'org D usage receipt is not correctly attributed to org-d-a2a' \
		'org D delegation minted a receipt misattributed to org-b-a2a' \
		'expect_a2a_status_d org-d-exhausted-reservation 429' \
		'create_federated_room' \
		'denied control join' \
		'send_signed_federation_probe' \
		'SYNAPSE_SIGNING_KEY' \
		'from signedjson.key import read_signing_keys' \
		'from signedjson.sign import sign_json' \
		'edu_type: "m.typing"' \
		'signed federation positive control' \
		'denied control federation send to' \
		'whitelist | sort) == ([$a, $b, $d, $c] | sort)' \
		'SYNAPSE_REGISTRATION_SHARED_SECRET' \
		'whitelist_enabled == true' \
		'federation-a-to-bd-' \
		'federation-b-to-ad-' \
		'federation-d-to-ab-' \
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
		rg --fixed-strings -- "${contract}" "${LIFECYCLE}" "${SEED_SOURCES[@]}" >/dev/null \
			|| fail "federation acceptance proof omits ${contract}"
	done
	quota_line="$(rg --line-number --fixed-strings \
		'expect_a2a_status exhausted-reservation-quota 429' "${a2a_seed}" | cut -d: -f1)"
	receipt_verify_line="$(rg --line-number --fixed-strings \
		'"${ROOT_DIR}/scripts/usage-receipt.sh" verify --input "${receipt}"' \
		"${a2a_seed}" | cut -d: -f1)"
	if ! [[ "${quota_line}" =~ ^[1-9][0-9]*$ && "${receipt_verify_line}" =~ ^[1-9][0-9]*$ ]] \
		|| ((quota_line >= receipt_verify_line)); then
		fail 'quota denial must run before local receipt verification can outlive the minute window'
	fi
	if rg --fixed-strings '403 | 404' "${SEED_SOURCES[@]}" >/dev/null; then
		fail 'federation acceptance treats a local missing-room response as a denied federation send'
	fi
	if rg --fixed-strings -- '--data-urlencode "client_secret=' "${SEED_SOURCES[@]}" >/dev/null; then
		fail 'federation acceptance exposes a confidential-client secret in process arguments'
	fi
	if rg --fixed-strings '%3N' "${SEED_SOURCES[@]}" >/dev/null; then
		fail 'federation acceptance depends on GNU date nanosecond formatting'
	fi
	rg --fixed-strings 'LLM_PROVIDER="demo"' "${DEMO_SOURCES[@]}" >/dev/null \
		|| fail 'federation profile can select a paid model provider'

}
