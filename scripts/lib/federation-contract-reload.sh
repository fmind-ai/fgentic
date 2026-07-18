#!/usr/bin/env bash
# Definition-only federation reload contracts sourced by scripts/test-federation.sh.
check_federation_reload() {
	# The reload drill must make its allow revision only in the disposable Git snapshot, reconcile it
	# through the normal source, prove both Synapse pods survive, and restore the tracked deny policy.
	for contract in \
		'FEDERATION_POLICY_PATH="apps/synapse-federation-policy/policy/policy.json"' \
		'local policy_file="${SNAPSHOT_DIR}/${FEDERATION_POLICY_PATH}"' \
		'.allowed_event_types |= (. + [$event_type] | unique)' \
		'mv "${next_policy}" "${policy_file}"' \
		'canonical federation policy must deny' \
		'init --quiet --object-format=sha1 --initial-branch main' \
		'expected_revision="main@sha1:${SOURCE_REVISION}"' \
		'[ "${actual_revision}" = "${expected_revision}" ]' \
		'Flux fetched exact ephemeral revision'; do
		rg --fixed-strings "${contract}" "${DEMO_SOURCES[@]}" >/dev/null \
			|| fail "ephemeral federation policy snapshot omits ${contract}"
	done
	for contract in \
		'FGENTIC_FED_POLICY_PROBE=deny "${ROOT_DIR}/scripts/federation.sh" up' \
		'FGENTIC_FED_POLICY_PROBE=allow "${ROOT_DIR}/scripts/federation.sh" up' \
		'SYNAPSE_A_UID="$(synapse_pod_uid matrix)"' \
		'SYNAPSE_B_UID="$(synapse_pod_uid matrix-b)"' \
		'assert_synapse_uids allow' \
		'assert_synapse_uids deny' \
		'Policy reload drill failed; deleting the disposable federation lab.' \
		'canonical deny remains running'; do
		rg --fixed-strings "${contract}" "${RELOAD}" >/dev/null \
			|| fail "federation policy reload drill omits ${contract}"
	done
	rg --fixed-strings '[tasks."fed:policy-reload"]' "${ROOT_DIR}/mise.toml" >/dev/null \
		|| fail 'mise task fed:policy-reload is missing'
	if rg --regexp 'kubectl.*(patch|replace).*fgentic-federation-policy' \
		"${DEMO_SOURCES[@]}" "${RELOAD}" >/dev/null; then
		fail 'policy reload bypasses Git and mutates the live ConfigMap directly'
	fi

}
