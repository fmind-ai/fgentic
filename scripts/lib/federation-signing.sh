#!/usr/bin/env bash
# Definition-only signed federation request helpers sourced by scripts/seed-federation.sh.
sign_federation_request() {
	local destination="$1"
	local uri="$2"
	local content="$3"
	local output_variable="$4"
	local signable signature_line key_id signature
	signable="$(jq --null-input --compact-output \
		--arg method PUT --arg uri "${uri}" --arg origin "${SERVER_C}" \
		--arg destination "${destination}" --argjson content "${content}" \
		'{method: $method, uri: $uri, origin: $origin,
      destination: $destination, content: $content}')"
	# The private key never leaves Synapse C. Its pinned signedjson dependency produces the exact
	# canonical Matrix signature; only the public key id and request signature return to the host.
	signature_line="$(printf '%s' "${signable}" \
		| kubectl --namespace matrix-c exec --stdin statefulset/ess-synapse-main -- \
			python -c '
import json
import sys

from signedjson.key import read_signing_keys
from signedjson.sign import sign_json

with open("/secrets/ess-generated/SYNAPSE_SIGNING_KEY", encoding="utf-8") as stream:
    signing_key = read_signing_keys(stream)[0]
document = json.load(sys.stdin)
origin = document["origin"]
signed = sign_json(document, origin, signing_key)
key_id = f"{signing_key.alg}:{signing_key.version}"
print(key_id + "\t" + signed["signatures"][origin][key_id])
')"
	IFS=$'\t' read -r key_id signature <<<"${signature_line}"
	[[ "${key_id}" =~ ^ed25519:[A-Za-z0-9_-]+$ ]] || die "invalid federation signing key id"
	[[ "${signature}" =~ ^[A-Za-z0-9_+/=-]+$ ]] || die "invalid federation request signature"
	printf -v "${output_variable}" \
		'X-Matrix origin="%s",destination="%s",key="%s",sig="%s"' \
		"${SERVER_C}" "${destination}" "${key_id}" "${signature}"
}
send_signed_federation_probe() {
	local destination="$1"
	local matrix_url="$2"
	local room_id="$3"
	local output="$4"
	local status_variable="$5"
	local transaction_id uri body authorization status origin_server_ts
	transaction_id="c-probe-${RANDOM}-$$"
	uri="/_matrix/federation/v1/send/${transaction_id}"
	origin_server_ts="$(($(date +%s) * 1000))"
	body="$(jq --null-input --compact-output \
		--arg destination "${destination}" --arg origin "${SERVER_C}" \
		--arg room_id "${room_id}" --arg user_id "@charlie:${SERVER_C}" \
		--argjson timestamp "${origin_server_ts}" '{
      destination: $destination,
      origin: $origin,
      origin_server_ts: $timestamp,
      pdus: [],
      edus: [{
        edu_type: "m.typing",
        content: {room_id: $room_id, user_id: $user_id, typing: true}
      }]
    }')"
	sign_federation_request "${destination}" "${uri}" "${body}" authorization
	status="$(request_status "${output}" --request PUT \
		--header "Authorization: ${authorization}" \
		--header 'Content-Type: application/json' --data "${body}" \
		"${matrix_url}${uri}")"
	printf -v "${status_variable}" '%s' "${status}"
}
