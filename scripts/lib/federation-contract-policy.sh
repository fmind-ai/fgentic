#!/usr/bin/env bash
# Definition-only federation policy contracts sourced by scripts/test-federation.sh.
check_federation_policy() {
	# The A homeserver is patched only in this disposable overlay. Its outbound trust and domain
	# restrictions must mirror B, otherwise one direction of the lab can silently become open.
	yq --unwrapScalar '
  .patches[] | select(.target.kind == "HelmRelease" and .target.name == "matrix-stack") | .patch
' "${MATRIX_A_COMPONENT}" >"${WORK_DIR}/homeserver-a-patch.yaml"
	yq --unwrapScalar '
  .[] | select(.path == "/spec/values/synapse/additional") |
  .value."10-federation".config
' "${WORK_DIR}/homeserver-a-patch.yaml" >"${WORK_DIR}/homeserver-a-config.yaml"
	yq --unwrapScalar '
  .[] | select(.path == "/spec/values/synapse/additional") |
  .value."20-federation-policy".config
' "${WORK_DIR}/homeserver-a-patch.yaml" >"${WORK_DIR}/homeserver-a-policy-config.yaml"
	for contract in \
		'default_room_version: "12"' \
		'federation_domain_whitelist:' \
		'${server_name}' \
		'${federation_partner_server_name}' \
		'federation_custom_ca_list:' \
		'/etc/fgentic-ca/ca.crt' \
		'ip_range_whitelist:' \
		'${federation_gateway_ip}' \
		'trusted_key_servers:' \
		'server_name: ${federation_partner_server_name}' \
		'/32'; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-a-config.yaml" >/dev/null \
			|| fail "homeserver A federation config omits ${contract}"
	done
	assert_yq \
		'.default_room_version == "12" and
    (.federation_domain_whitelist | length) == 2 and
    .federation_domain_whitelist[0] == "${server_name}" and
    .federation_domain_whitelist[1] == "${federation_partner_server_name}" and
    (.trusted_key_servers | length) == 1 and
    .trusted_key_servers[0].server_name == "${federation_partner_server_name}"' \
		"${WORK_DIR}/homeserver-a-config.yaml" \
		'homeserver A does not have an exact A/B domain allowlist, room v12 default, and B key notary'
	if rg --fixed-strings '${federation_denied_server_name}' \
		"${WORK_DIR}/homeserver-a-config.yaml" >/dev/null; then
		fail 'homeserver A federation allowlist includes denied homeserver C'
	fi
	for contract in \
		'fgentic_federation_policy.FederationPolicyModule' \
		'policy_path: /etc/fgentic/federation-policy/policy.json'; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-a-policy-config.yaml" >/dev/null \
			|| fail "homeserver A policy module config omits ${contract}"
	done

	for contract in \
		'../../../apps/synapse-federation-policy' \
		'name: fgentic-local-ca' \
		'name: fgentic-synapse-federation-policy-v1' \
		'name: fgentic-federation-policy' \
		'mountPath: /etc/fgentic-ca' \
		'mountPath: /opt/fgentic/synapse-modules' \
		'mountPath: /etc/fgentic/federation-policy' \
		'name: PYTHONPATH' \
		'value: /opt/fgentic/synapse-modules' \
		'readOnly: true' \
		'${federation_denied_server_name}' \
		'matrix.${federation_denied_server_name}' \
		'path: /spec/values/synapse/hostAliases'; do
		rg --fixed-strings "${contract}" "${MATRIX_A_COMPONENT}" >/dev/null \
			|| fail "homeserver A runtime trust wiring omits ${contract}"
	done

	kubectl kustomize "${MATRIX_B_LAYER}" >"${WORK_DIR}/matrix-b.yaml"
	kubectl kustomize "${MATRIX_C_LAYER}" >"${WORK_DIR}/matrix-c.yaml"
	kubectl kustomize "${NAMESPACE_COMPONENT}" >"${WORK_DIR}/namespaces.yaml"
	kubectl kustomize "${POSTGRES_COMPONENT}" >"${WORK_DIR}/postgres.yaml"

	assert_yq \
		'select(.kind == "ConfigMap" and .metadata.name == "fgentic-synapse-federation-policy-v1") |
    .metadata.namespace == "matrix-b" and .immutable == true and
    (.data."fgentic_federation_policy.py" | contains("class FederationPolicyModule"))' \
		"${WORK_DIR}/matrix-b.yaml" 'homeserver B does not receive the immutable policy module source'
	assert_yq \
		'select(.kind == "ConfigMap" and .metadata.name == "fgentic-federation-policy") |
    .metadata.namespace == "matrix-b" and .immutable == null and
    (.data."policy.json" | from_json | .version == 1)' \
		"${WORK_DIR}/matrix-b.yaml" 'homeserver B does not receive the reloadable versioned policy'

	jq -e --arg a 'org-a.fgentic.localhost' --arg b 'org-b.fgentic.localhost' \
		--arg blocked 'com.fgentic.blocked' '
  keys == ["allowed_event_types", "allowed_servers", "invite_rule", "version"] and
  .version == 1 and .allowed_servers == [$a, $b] and
  .invite_rule == "allow_from_allowed_servers" and
  (.allowed_event_types | index($blocked)) == null and
  (["m.room.create", "m.room.join_rules", "m.room.member", "m.room.message",
    "m.room.power_levels", "m.room.server_acl"] - .allowed_event_types | length) == 0
' "${POLICY_DOCUMENT}" >/dev/null || fail 'canonical federation policy is not exact and deny-by-default'
	for contract in \
		'should_drop_federated_event' \
		'federated_user_may_invite' \
		'event_type_not_allowed' \
		'run_db_interaction' \
		'federation_inbound_events_staging' \
		'WHERE room_id = ? AND event_id = ?' \
		'fgentic_federation_policy_staged_event_grandfathered' \
		'fgentic_federation_policy_violation' \
		'allowed_event_type_count' \
		'allowed_server_count' \
		'policy_digest'; do
		rg --fixed-strings "${contract}" "${POLICY_MODULE}" >/dev/null \
			|| fail "federation policy module omits ${contract}"
	done

	assert_yq \
		'select(.kind == "Namespace" and .metadata.name == "matrix-b") |
    .metadata.labels."pod-security.kubernetes.io/enforce" == "baseline" and
    .metadata.labels."pod-security.kubernetes.io/audit" == "restricted" and
    .metadata.labels."pod-security.kubernetes.io/warn" == "restricted"' \
		"${WORK_DIR}/namespaces.yaml" 'matrix-b is not owned by the namespace layer with PSS labels'
	assert_yq \
		'select(.kind == "Namespace" and .metadata.name == "matrix-c") |
    .metadata.labels."pod-security.kubernetes.io/enforce" == "baseline" and
    .metadata.labels."pod-security.kubernetes.io/audit" == "restricted" and
    .metadata.labels."pod-security.kubernetes.io/warn" == "restricted"' \
		"${WORK_DIR}/namespaces.yaml" 'matrix-c is not owned by the namespace layer with PSS labels'

	assert_yq \
		'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
    .metadata.namespace == "matrix-b" and
    .spec.releaseName == "ess" and
    .spec.values.serverName == "${federation_partner_server_name}" and
    .spec.values.synapse.postgres.host == "platform-pg-rw.postgres.svc.cluster.local" and
    .spec.values.synapse.postgres.user == "synapse_b" and
    .spec.values.synapse.postgres.database == "synapse_b" and
    .spec.values.synapse.postgres.sslMode == "require" and
    .spec.values.synapse.postgres.password.secret == "pg-synapse-b" and
    .spec.values.matrixAuthenticationService.enabled == false and
    .spec.values.elementWeb.enabled == false and
    .spec.values.elementAdmin.enabled == false and
    .spec.values.matrixRTC.enabled == false and
    .spec.values.wellKnownDelegation.enabled == true and
    .spec.values.postgres.enabled == false and
    ([.spec.values.synapse.hostAliases[] |
      select(.ip == "${federation_gateway_ip}") | .hostnames[]] |
      contains(["${server_name}", "matrix.${server_name}",
        "${federation_partner_server_name}", "matrix.${federation_partner_server_name}",
        "${federation_denied_server_name}", "matrix.${federation_denied_server_name}"])) and
    ([.spec.values.synapse.extraVolumes[] |
      select(.configMap.name == "fgentic-local-ca")] | length) == 1 and
    ([.spec.values.synapse.extraVolumeMounts[] |
      select(.mountPath == "/etc/fgentic-ca" and .readOnly == true)] | length) == 1' \
		"${WORK_DIR}/matrix-b.yaml" 'homeserver B is not a minimal, locally trusted Synapse-only ESS release'

	yq --unwrapScalar '
  select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
  .spec.values.synapse.additional."10-federation".config
' "${WORK_DIR}/matrix-b.yaml" >"${WORK_DIR}/homeserver-b-config.yaml"
	yq --unwrapScalar '
  select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
  .spec.values.synapse.additional."20-federation-policy".config
' "${WORK_DIR}/matrix-b.yaml" >"${WORK_DIR}/homeserver-b-policy-config.yaml"
	for contract in \
		'default_room_version: "12"' \
		'federation_domain_whitelist:' \
		'${server_name}' \
		'${federation_partner_server_name}' \
		'federation_custom_ca_list:' \
		'/etc/fgentic-ca/ca.crt' \
		'ip_range_whitelist:' \
		'${federation_gateway_ip}' \
		'trusted_key_servers:' \
		'server_name: ${server_name}' \
		'/32'; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-b-config.yaml" >/dev/null \
			|| fail "homeserver B federation config omits ${contract}"
	done
	assert_yq \
		'.default_room_version == "12" and
    (.federation_domain_whitelist | length) == 2 and
    .federation_domain_whitelist[0] == "${server_name}" and
    .federation_domain_whitelist[1] == "${federation_partner_server_name}" and
    (.trusted_key_servers | length) == 1 and
    .trusted_key_servers[0].server_name == "${server_name}"' \
		"${WORK_DIR}/homeserver-b-config.yaml" \
		'homeserver B does not have an exact A/B domain allowlist, room v12 default, and A key notary'
	if rg --fixed-strings '${federation_denied_server_name}' \
		"${WORK_DIR}/homeserver-b-config.yaml" >/dev/null; then
		fail 'homeserver B federation allowlist includes denied homeserver C'
	fi
	for contract in \
		'fgentic_federation_policy.FederationPolicyModule' \
		'policy_path: /etc/fgentic/federation-policy/policy.json'; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-b-policy-config.yaml" >/dev/null \
			|| fail "homeserver B policy module config omits ${contract}"
	done
	assert_yq \
		'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
    ([.spec.values.synapse.extraVolumes[] | select(
      .configMap.name == "fgentic-synapse-federation-policy-v1" or
      .configMap.name == "fgentic-federation-policy")] | length) == 2 and
    ([.spec.values.synapse.extraVolumeMounts[] | select(
      .mountPath == "/opt/fgentic/synapse-modules" or
      .mountPath == "/etc/fgentic/federation-policy")] | length) == 2 and
    ([.spec.values.synapse.extraEnv[] | select(
      .name == "PYTHONPATH" and .value == "/opt/fgentic/synapse-modules")] | length) == 1' \
		"${WORK_DIR}/matrix-b.yaml" 'homeserver B policy source, data, or Python path is not mounted'

	assert_yq \
		'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-c") |
    .metadata.namespace == "matrix-c" and
    .spec.releaseName == "ess" and
    .spec.values.serverName == "${federation_denied_server_name}" and
    .spec.values.synapse.postgres.host == "platform-pg-rw.postgres.svc.cluster.local" and
    .spec.values.synapse.postgres.user == "synapse_c" and
    .spec.values.synapse.postgres.database == "synapse_c" and
    .spec.values.synapse.postgres.sslMode == "require" and
    .spec.values.synapse.postgres.password.secret == "pg-synapse-c" and
    .spec.values.matrixAuthenticationService.enabled == false and
    .spec.values.elementWeb.enabled == false and
    .spec.values.elementAdmin.enabled == false and
    .spec.values.matrixRTC.enabled == false and
    .spec.values.wellKnownDelegation.enabled == true and
    .spec.values.postgres.enabled == false and
    ([.spec.values.synapse.hostAliases[] |
      select(.ip == "${federation_gateway_ip}") | .hostnames[]] |
      contains(["${server_name}", "matrix.${server_name}",
        "${federation_partner_server_name}", "matrix.${federation_partner_server_name}",
        "${federation_denied_server_name}", "matrix.${federation_denied_server_name}"])) and
    ([.spec.values.synapse.extraVolumes[] |
      select(.configMap.name == "fgentic-local-ca")] | length) == 1 and
    ([.spec.values.synapse.extraVolumeMounts[] |
      select(.mountPath == "/etc/fgentic-ca" and .readOnly == true)] | length) == 1' \
		"${WORK_DIR}/matrix-c.yaml" \
		'homeserver C is not a minimal, locally trusted Synapse-only ESS release'
	yq --unwrapScalar '
  select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-c") |
  .spec.values.synapse.additional."10-federation".config
' "${WORK_DIR}/matrix-c.yaml" >"${WORK_DIR}/homeserver-c-config.yaml"
	assert_yq \
		'.default_room_version == "12" and
    (.federation_domain_whitelist | length) == 3 and
    .federation_domain_whitelist[0] == "${server_name}" and
    .federation_domain_whitelist[1] == "${federation_partner_server_name}" and
    .federation_domain_whitelist[2] == "${federation_denied_server_name}" and
    (.trusted_key_servers | length) == 1 and
    .trusted_key_servers[0].server_name == "${server_name}"' \
		"${WORK_DIR}/homeserver-c-config.yaml" \
		'homeserver C does not have room v12, controlled local peers, and A key notary'
	if rg --fixed-strings -e '10.0.0.0/8' -e '172.16.0.0/12' -e '192.168.0.0/16' \
		"${WORK_DIR}/homeserver-a-config.yaml" "${WORK_DIR}/homeserver-b-config.yaml" \
		"${WORK_DIR}/homeserver-c-config.yaml" >/dev/null; then
		fail 'federation config trusts a broad private network instead of the ingress /32'
	fi

	for homeserver in \
		"${WORK_DIR}/homeserver-a-config.yaml" \
		"${WORK_DIR}/homeserver-b-config.yaml" \
		"${WORK_DIR}/homeserver-c-config.yaml"; do
		if rg --regexp "^[[:space:]]*-[[:space:]]*['\"]?\\*" "${homeserver}" >/dev/null; then
			fail "${homeserver##*/} contains a wildcard federation allowlist entry"
		fi
	done

	assert_yq \
		'select(.kind == "Database" and .spec.name == "synapse_b") |
    .metadata.namespace == "postgres" and
    .spec.cluster.name == "platform-pg" and
    .spec.owner == "synapse_b" and
    .spec.encoding == "UTF8" and
    .spec.localeCollate == "C" and
    .spec.localeCType == "C" and
    .spec.template == "template0"' \
		"${WORK_DIR}/postgres.yaml" 'homeserver B does not have a C-locale, role-scoped CNPG database'
	assert_yq \
		'select(.kind == "Database" and .spec.name == "synapse_c") |
    .metadata.name == "synapse-c" and
    .metadata.namespace == "postgres" and
    .spec.cluster.name == "platform-pg" and
    .spec.owner == "synapse_c" and
    .spec.encoding == "UTF8" and
    .spec.localeCollate == "C" and
    .spec.localeCType == "C" and
    .spec.template == "template0"' \
		"${WORK_DIR}/postgres.yaml" 'homeserver C does not have a C-locale, role-scoped CNPG database'

	yq --unwrapScalar '.patches[]?.patch' \
		"${POSTGRES_COMPONENT}/kustomization.yaml" >"${WORK_DIR}/postgres-patches.yaml"
	for contract in \
		'name: synapse_b' \
		'name: pg-synapse-b' \
		'name: synapse_c' \
		'name: pg-synapse-c' \
		'hostssl synapse_b synapse_b all scram-sha-256' \
		'hostssl synapse_c synapse_c all scram-sha-256' \
		'hostssl all all all reject' \
		'hostnossl all all all reject'; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/postgres-patches.yaml" >/dev/null \
			|| fail "homeserver B database boundary omits ${contract}"
	done

	assert_yq \
		'select(.kind == "Certificate" and .metadata.name == "matrix-b-tls") |
    .metadata.namespace == "gateway" and
    .spec.secretName == "matrix-b-tls" and
    .spec.issuerRef.kind == "ClusterIssuer" and
    .spec.issuerRef.name == "${cluster_issuer}" and
    (.spec.dnsNames | contains(["${federation_partner_server_name}",
      "matrix.${federation_partner_server_name}",
      "id.${federation_partner_server_name}"]))' \
		"${WORK_DIR}/matrix-b.yaml" 'homeserver B does not have a local-CA leaf certificate'
	assert_yq \
		'select(.kind == "Gateway" and .metadata.name == "federation-b") |
    .metadata.namespace == "gateway" and
    .spec.gatewayClassName == "traefik" and
    ([.spec.listeners[] | select(.protocol == "HTTPS") | .hostname] |
      contains(["${federation_partner_server_name}",
        "matrix.${federation_partner_server_name}",
        "id.${federation_partner_server_name}"])) and
    ([.spec.listeners[].tls.certificateRefs[] | select(.name == "matrix-b-tls")] |
      length) == 3 and
    ([.spec.listeners[] | select(
      .allowedRoutes.namespaces.from == "Selector" and
      .allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "matrix-b"
    )] | length) == 2 and
    ([.spec.listeners[] | select(
      .name == "https-id-b" and
      .allowedRoutes.namespaces.from == "Selector" and
      .allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "keycloak"
    )] | length) == 1' \
		"${WORK_DIR}/matrix-b.yaml" \
		'homeserver B has no isolated local-CA Gateway listeners for Matrix and Keycloak'
	assert_yq \
		'select(.kind == "HTTPRoute" and .metadata.name == "synapse-b") |
    .metadata.namespace == "matrix-b" and
    .spec.parentRefs[0].name == "federation-b" and
    (.spec.hostnames | contains(["matrix.${federation_partner_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-synapse" and .port == 8008)] | length) == 1' \
		"${WORK_DIR}/matrix-b.yaml" 'homeserver B Synapse route is incomplete'
	assert_yq \
		'select(.kind == "HTTPRoute" and .metadata.name == "well-known-b") |
    .metadata.namespace == "matrix-b" and
    .spec.parentRefs[0].name == "federation-b" and
    (.spec.hostnames | contains(["${federation_partner_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-well-known" and .port == 8010)] | length) == 1' \
		"${WORK_DIR}/matrix-b.yaml" 'homeserver B well-known delegation route is incomplete'

	assert_yq \
		'select(.kind == "Certificate" and .metadata.name == "matrix-c-tls") |
    .metadata.namespace == "gateway" and
    .spec.secretName == "matrix-c-tls" and
    .spec.issuerRef.kind == "ClusterIssuer" and
    .spec.issuerRef.name == "${cluster_issuer}" and
    (.spec.dnsNames | contains(["${federation_denied_server_name}",
      "matrix.${federation_denied_server_name}"]))' \
		"${WORK_DIR}/matrix-c.yaml" 'homeserver C does not have a local-CA leaf certificate'
	assert_yq \
		'select(.kind == "Gateway" and .metadata.name == "federation-c") |
    .metadata.namespace == "gateway" and
    .spec.gatewayClassName == "traefik" and
    ([.spec.listeners[] | select(.protocol == "HTTPS") | .hostname] |
      contains(["${federation_denied_server_name}",
        "matrix.${federation_denied_server_name}"])) and
    ([.spec.listeners[].tls.certificateRefs[] | select(.name == "matrix-c-tls")] |
      length) == 2 and
    ([.spec.listeners[] | select(
      .allowedRoutes.namespaces.from == "Selector" and
      .allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "matrix-c"
    )] | length) == 2' \
		"${WORK_DIR}/matrix-c.yaml" 'homeserver C has no local-CA TLS Gateway for apex and Synapse'
	assert_yq \
		'select(.kind == "HTTPRoute" and .metadata.name == "synapse-c") |
    .metadata.namespace == "matrix-c" and
    .spec.parentRefs[0].name == "federation-c" and
    (.spec.hostnames | contains(["matrix.${federation_denied_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-synapse" and .port == 8008)] | length) == 1' \
		"${WORK_DIR}/matrix-c.yaml" 'homeserver C Synapse route is incomplete'
	assert_yq \
		'select(.kind == "HTTPRoute" and .metadata.name == "well-known-c") |
    .metadata.namespace == "matrix-c" and
    .spec.parentRefs[0].name == "federation-c" and
    (.spec.hostnames | contains(["${federation_denied_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-well-known" and .port == 8010)] | length) == 1' \
		"${WORK_DIR}/matrix-c.yaml" 'homeserver C well-known delegation route is incomplete'

}
