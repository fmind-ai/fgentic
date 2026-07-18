#!/usr/bin/env bash
# Definition-only Matrix federation proof helpers sourced by scripts/seed-federation.sh.
register_user() {
	local namespace="$1"
	local matrix_url="$2"
	local username="$3"
	local display_name="$4"
	local password="$5"
	local secret nonce nonce_response mac digest document status registration_token
	local response="${WORK_DIR}/register-${username}.json"

	secret="$(kubectl --namespace "${namespace}" get secret ess-generated \
		--output 'go-template={{index .data "SYNAPSE_REGISTRATION_SHARED_SECRET" | base64decode}}')"
	nonce_response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		"${matrix_url}/_synapse/admin/v1/register")"
	nonce="$(jq -er '.nonce' <<<"${nonce_response}")"
	digest="$(printf '%s\0%s\0%s\0%s' "${nonce}" "${username}" "${password}" notadmin \
		| openssl sha1 -hmac "${secret}")"
	mac="${digest##* }"
	document="$(jq --null-input --compact-output \
		--arg nonce "${nonce}" --arg username "${username}" --arg displayname "${display_name}" \
		--arg password "${password}" --arg mac "${mac}" \
		'{nonce: $nonce, username: $username, displayname: $displayname, password: $password,
      admin: false, mac: $mac}')"
	status="$(request_status "${response}" --request POST \
		--header 'Content-Type: application/json' --data "${document}" \
		"${matrix_url}/_synapse/admin/v1/register")"
	secret=""
	document=""
	case "${status}" in
		200)
			# The registration API creates a bootstrap session. Revoke it; the proof logs in normally.
			registration_token="$(jq -r '.access_token // empty' "${response}")"
			if [ -n "${registration_token}" ]; then
				curl --silent --show-error --cacert "${CA_CERT}" --request POST \
					--header "Authorization: Bearer ${registration_token}" \
					"${matrix_url}/_matrix/client/v3/logout" >/dev/null
				registration_token=""
			fi
			;;
		400)
			jq -e '.errcode == "M_USER_IN_USE"' "${response}" >/dev/null \
				|| die "${username} registration failed (HTTP 400)"
			;;
		*) die "${username} registration failed (HTTP ${status})" ;;
	esac
}
login_user() {
	local matrix_url="$1"
	local username="$2"
	local password="$3"
	local token_variable="$4"
	local document response token
	document="$(jq --null-input --compact-output --arg username "${username}" \
		--arg password "${password}" '{
      type: "m.login.password",
      identifier: {type: "m.id.user", user: $username},
      password: $password,
      initial_device_display_name: "Fgentic federation proof"
    }')"
	response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header 'Content-Type: application/json' --data "${document}" \
		"${matrix_url}/_matrix/client/v3/login")"
	token="$(jq -er '.access_token | select(type == "string" and length > 0)' <<<"${response}")"
	printf -v "${token_variable}" '%s' "${token}"
}

verify_server() {
	local server="$1"
	local matrix_url="https://matrix.${server}"
	local expected="matrix.${server}:443"
	local response
	response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		"https://${server}/.well-known/matrix/server")"
	jq -e --arg expected "${expected}" '."m.server" == $expected' <<<"${response}" >/dev/null
	response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		"${matrix_url}/_matrix/federation/v1/version")"
	jq -e '.server.name | type == "string" and length > 0' <<<"${response}" >/dev/null
	response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		"${matrix_url}/_matrix/key/v2/server")"
	jq -e --arg server "${server}" '.server_name == $server' <<<"${response}" >/dev/null
}

verify_whitelist() {
	local matrix_url="$1"
	local token="$2"
	local response
	response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_synapse/client/v1/config/federation_whitelist")"
	jq -e --arg a "${SERVER_A}" --arg b "${SERVER_B}" \
		'.whitelist_enabled == true and (.whitelist | sort) == ([$a, $b] | sort)' \
		<<<"${response}" >/dev/null
}

verify_control_whitelist() {
	local matrix_url="$1"
	local token="$2"
	local response
	response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_synapse/client/v1/config/federation_whitelist")"
	jq -e --arg a "${SERVER_A}" --arg b "${SERVER_B}" --arg c "${SERVER_C}" \
		'.whitelist_enabled == true and (.whitelist | sort) == ([$a, $b, $c] | sort)' \
		<<<"${response}" >/dev/null
}

wait_for_mounted_policy_mode() {
	local namespace="$1"
	local deadline=$((SECONDS + 180))
	local policy
	while ((SECONDS < deadline)); do
		policy="$(kubectl --namespace "${namespace}" exec statefulset/ess-synapse-main -- \
			cat /etc/fgentic/federation-policy/policy.json 2>/dev/null || true)"
		if jq -e --arg type "${POLICY_EVENT_TYPE}" --arg mode "${POLICY_PROBE_MODE}" '
	      .version == 1 and
	      if $mode == "allow" then
	        (.allowed_event_types | index($type)) != null
	      else
	        (.allowed_event_types | index($type)) == null
	      end
	    ' <<<"${policy}" >/dev/null 2>&1; then
			return
		fi
		sleep 2
	done
	die "${namespace} did not project federation policy mode ${POLICY_PROBE_MODE} within 3 minutes"
}

initial_sync_token() {
	local matrix_url="$1"
	local token="$2"
	local response
	response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/sync?timeout=0")"
	jq -er '.next_batch | select(type == "string" and length > 0)' <<<"${response}"
}

wait_for_event() {
	local matrix_url="$1"
	local token="$2"
	local room_id="$3"
	local since="$4"
	local event_id="$5"
	local sender="$6"
	local body="$7"
	local deadline=$((SECONDS + 180))
	local encoded_since sync
	while ((SECONDS < deadline)); do
		encoded_since="$(jq --null-input --raw-output --arg value "${since}" '$value | @uri')"
		sync="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
			--header "Authorization: Bearer ${token}" \
			"${matrix_url}/_matrix/client/v3/sync?timeout=1000&since=${encoded_since}")"
		if jq -e --arg room_id "${room_id}" --arg event_id "${event_id}" \
			--arg sender "${sender}" --arg body "${body}" '
        .rooms.join[$room_id].timeline.events[]? | select(
          .event_id == $event_id and .sender == $sender and
          .type == "m.room.message" and .content.msgtype == "m.text" and .content.body == $body)
      ' <<<"${sync}" >/dev/null; then
			return
		fi
		since="$(jq -er '.next_batch | select(type == "string" and length > 0)' <<<"${sync}")"
		sleep 2
	done
	die "federated event ${event_id} did not arrive within 3 minutes"
}

verify_membership() {
	local matrix_url="$1"
	local token="$2"
	local encoded_room="$3"
	local deadline=$((SECONDS + 120))
	local joined
	while ((SECONDS < deadline)); do
		joined="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
			--header "Authorization: Bearer ${token}" \
			"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/joined_members")"
		if jq -e --arg alice "@alice:${SERVER_A}" --arg bob "@bob:${SERVER_B}" \
			'.joined | has($alice) and has($bob)' <<<"${joined}" >/dev/null; then
			return
		fi
		sleep 2
	done
	die "both users were not joined on ${matrix_url} within 2 minutes"
}

verify_federated_room_policy() {
	local matrix_url="$1"
	local token="$2"
	local encoded_room="$3"
	local acl creation join_rules
	acl="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.server_acl")"
	jq -e --arg a "${SERVER_A}" --arg b "${SERVER_B}" '
    .allow_ip_literals == false and .deny == [] and
    (.allow | sort) == ([$a, $b] | sort)
  ' <<<"${acl}" >/dev/null || die "federated room server ACL is not partner-only"

	creation="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.create")"
	jq -e '.room_version == "12" and ."m.federate" == true' <<<"${creation}" >/dev/null \
		|| die "federated room is not explicitly federated at room version 12"

	join_rules="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.join_rules")"
	jq -e '.join_rule == "public"' <<<"${join_rules}" >/dev/null \
		|| die "federated proof room is not public for the negative join test"
}

verify_denied_membership() {
	local matrix_url="$1"
	local token="$2"
	local encoded_room="$3"
	local joined
	joined="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/joined_members")"
	jq -e --arg charlie "@charlie:${SERVER_C}" '.joined | has($charlie) | not' \
		<<<"${joined}" >/dev/null || die "denied control user joined the federated room"
}

expect_forbidden() {
	local description="$1"
	local output="$2"
	shift 2
	local status
	status="$(request_status "${output}" "$@")"
	assert_forbidden_response "${description}" "${status}" "${output}"
}

assert_forbidden_response() {
	local description="$1"
	local status="$2"
	local output="$3"
	[ "${status}" = "403" ] || die "${description} was not forbidden (HTTP ${status})"
	jq -e '.errcode == "M_FORBIDDEN" and (.error | type == "string" and length > 0)' \
		"${output}" >/dev/null || die "${description} did not return a Matrix forbidden error"
}

create_federated_room() {
	local output_variable="$1"
	local room_name="${2:-Fgentic Federation Proof}"
	local document response created_room_id
	# This is the lab's only supported federated-room constructor: the ACL is initial state, so no
	# federated event can race ahead of the participant-only policy.
	document="$(jq --null-input --compact-output --arg a "${SERVER_A}" --arg b "${SERVER_B}" \
		--arg name "${room_name}" '{
	    name: $name,
	    preset: "private_chat",
	    visibility: "private",
    room_version: "12",
    creation_content: {"m.federate": true},
    initial_state: [
      {
        type: "m.room.join_rules",
        state_key: "",
        content: {join_rule: "public"}
      },
      {
        type: "m.room.server_acl",
        state_key: "",
        content: {allow: [$a, $b], deny: [], allow_ip_literals: false}
      }
    ]
  }')"
	response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${ALICE_TOKEN}" \
		--header 'Content-Type: application/json' --data "${document}" \
		"${MATRIX_A_URL}/_matrix/client/v3/createRoom")"
	created_room_id="$(jq -er '.room_id' <<<"${response}")"
	printf -v "${output_variable}" '%s' "${created_room_id}"
}

invite_and_join_partner() {
	local encoded_room="$1"
	local invite_document encoded_a
	invite_document="$(jq --null-input --compact-output --arg user_id "@bob:${SERVER_B}" \
		'{user_id: $user_id}')"
	curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" --request POST \
		--header "Authorization: Bearer ${ALICE_TOKEN}" \
		--header 'Content-Type: application/json' --data "${invite_document}" \
		"${MATRIX_A_URL}/_matrix/client/v3/rooms/${encoded_room}/invite" >/dev/null
	encoded_a="$(jq --null-input --raw-output --arg value "${SERVER_A}" '$value | @uri')"
	curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" --request POST \
		--header "Authorization: Bearer ${BOB_TOKEN}" \
		--header 'Content-Type: application/json' --data '{}' \
		"${MATRIX_B_URL}/_matrix/client/v3/join/${encoded_room}?server_name=${encoded_a}" >/dev/null
	verify_membership "${MATRIX_A_URL}" "${ALICE_TOKEN}" "${encoded_room}"
	verify_membership "${MATRIX_B_URL}" "${BOB_TOKEN}" "${encoded_room}"
}

verify_local_policy_event() {
	local encoded_room="$1"
	local encoded_event="$2"
	local event_id="$3"
	local marker="$4"
	local event
	event="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${BOB_TOKEN}" \
		"${MATRIX_B_URL}/_matrix/client/v3/rooms/${encoded_room}/event/${encoded_event}")"
	jq -e --arg event_id "${event_id}" --arg sender "@bob:${SERVER_B}" \
		--arg type "${POLICY_EVENT_TYPE}" --arg marker "${marker}" '
	    .event_id == $event_id and .sender == $sender and .type == $type and
	    .content.probe_id == $marker
	  ' <<<"${event}" >/dev/null || die "homeserver B did not retain the local policy probe"
}

wait_for_policy_violation() {
	local room_id="$1"
	local event_id="$2"
	local marker="$3"
	local deadline=$((SECONDS + 180))
	local logs payload
	while ((SECONDS < deadline)); do
		logs="$(kubectl --namespace matrix logs statefulset/ess-synapse-main \
			--since=10m 2>/dev/null || true)"
		# Parse the module's structured record instead of matching a Matrix event ID as shell text.
		# Event IDs are opaque and commonly begin with `$`.
		payload="$(printf '%s\n' "${logs}" \
			| sed -n "s/^.*${POLICY_LOG_PREFIX}//p" \
			| jq --compact-output --arg event_id "${event_id}" \
				'select(.event == $event_id)' | tail -n 1 || true)"
		if [ -z "${payload}" ]; then
			sleep 2
			continue
		fi
		if rg --fixed-strings "${marker}" <<<"${logs}" >/dev/null; then
			die "federation policy logs exposed denied event content"
		fi
		jq -e --arg event_id "${event_id}" --arg room_id "${room_id}" \
			--arg server "${SERVER_B}" --arg type "${POLICY_EVENT_TYPE}" '
	      (keys | sort) == ([
	        "allowed_event_type_count", "allowed_server_count", "event", "invite_rule",
	        "policy_digest", "reason", "room", "server", "type"
	      ] | sort) and
	      .event == $event_id and .room == $room_id and .server == $server and
	      .type == $type and .reason == "event_type_not_allowed" and
	      .invite_rule == "allow_from_allowed_servers" and
	      (.allowed_event_type_count | type == "number" and . > 0) and
	      (.allowed_server_count | type == "number" and . == 2) and
	      (.policy_digest | type == "string" and test("^[0-9a-f]{64}$"))
	    ' <<<"${payload}" >/dev/null \
			|| die "federation policy violation log is not the content-free canonical record"
		return
	done
	die "homeserver A did not log policy denial for ${event_id} within 3 minutes"
}

verify_remote_policy_event_absent() {
	local encoded_room="$1"
	local encoded_event="$2"
	local event_id="$3"
	local response="${WORK_DIR}/policy-event-on-a.json"
	local deadline=$((SECONDS + 20))
	local status
	while ((SECONDS < deadline)); do
		status="$(request_status "${response}" \
			--header "Authorization: Bearer ${ALICE_TOKEN}" \
			"${MATRIX_A_URL}/_matrix/client/v3/rooms/${encoded_room}/event/${encoded_event}")"
		case "${status}" in
			404)
				jq -e '.errcode == "M_NOT_FOUND"' "${response}" >/dev/null \
					|| die "homeserver A returned a non-Matrix missing-event response"
				;;
			200) die "homeserver A retained policy-denied event ${event_id}" ;;
			*) die "homeserver A event lookup failed while proving denial (HTTP ${status})" ;;
		esac
		sleep 2
	done
}

wait_for_remote_policy_event() {
	local encoded_room="$1"
	local encoded_event="$2"
	local event_id="$3"
	local marker="$4"
	local response="${WORK_DIR}/policy-event-on-a.json"
	local deadline=$((SECONDS + 180))
	local status logs log_line
	while ((SECONDS < deadline)); do
		status="$(request_status "${response}" \
			--header "Authorization: Bearer ${ALICE_TOKEN}" \
			"${MATRIX_A_URL}/_matrix/client/v3/rooms/${encoded_room}/event/${encoded_event}")"
		case "${status}" in
			200)
				jq -e --arg event_id "${event_id}" --arg sender "@bob:${SERVER_B}" \
					--arg type "${POLICY_EVENT_TYPE}" --arg marker "${marker}" '
		        .event_id == $event_id and .sender == $sender and .type == $type and
		        .content.probe_id == $marker
		      ' "${response}" >/dev/null \
					|| die "homeserver A returned an invalid allowed policy probe"
				logs="$(kubectl --namespace matrix logs statefulset/ess-synapse-main \
					--since=10m 2>/dev/null || true)"
				while IFS= read -r log_line; do
					if [[ "${log_line}" == *"${POLICY_LOG_PREFIX}"* &&
						"${log_line}" == *"\"event\":\"${event_id}\""* ]]; then
						die "homeserver A logged an allowed policy probe as a violation"
					fi
				done <<<"${logs}"
				return
				;;
			404)
				jq -e '.errcode == "M_NOT_FOUND"' "${response}" >/dev/null \
					|| die "homeserver A returned a non-Matrix missing-event response"
				;;
			*) die "homeserver A event lookup failed while proving allow (HTTP ${status})" ;;
		esac
		sleep 2
	done
	die "policy-allowed event ${event_id} did not reach homeserver A within 3 minutes"
}
