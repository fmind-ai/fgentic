#!/usr/bin/env bash
# Definition-only federation signing contracts sourced by scripts/test-federation.sh.
# shellcheck disable=SC2016 # jq bindings and source-contract placeholders are intentionally literal
# shellcheck disable=SC2312 # substitutions feed fail-closed assertions or mandatory fixture execution
check_federation_signing() {
	# The receipt binary adds gRPC to the shipped image. Preserve its upstream NOTICE attribution in
	# the same file that the distroless image exposes with both production binaries.
	rg --fixed-strings 'Copyright 2014 gRPC authors.' \
		"${ROOT_DIR}/apps/matrix-a2a-bridge/NOTICE" >/dev/null \
		|| fail 'usage-receipt image NOTICE omits the gRPC attribution'
	rg --fixed-strings \
		'COPY NOTICE /usr/share/doc/matrix-a2a-bridge/NOTICE' \
		"${ROOT_DIR}/apps/matrix-a2a-bridge/Dockerfile" >/dev/null \
		|| fail 'bridge image does not ship its third-party NOTICE'

	# Exercise the same offline signer that the lifecycle uses. The fixture is rendered to its final
	# public domains before signing, then verified and tampered without ever writing a key in the repo.
	cp "${AGENT_CARD_TEMPLATE}" "${WORK_DIR}/unsigned-agent-card.json"
	CARD_SERVER=org-a.fgentic.localhost CARD_PARTNER=org-b.fgentic.localhost yq --inplace '
  (... | select(tag == "!!str")) |=
    sub("\\$\\{server_name\\}"; strenv(CARD_SERVER)) |
  (... | select(tag == "!!str")) |=
    sub("\\$\\{federation_partner_server_name\\}"; strenv(CARD_PARTNER))
' "${WORK_DIR}/unsigned-agent-card.json"
	if rg --regexp '\$\{[^}]+\}' "${WORK_DIR}/unsigned-agent-card.json" >/dev/null; then
		fail 'AgentCard signing fixture retained a post-sign substitution'
	fi
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "${WORK_DIR}/agent-card-private.pem" 2>/dev/null
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "${WORK_DIR}/usage-receipt-private.pem" 2>/dev/null
	chmod 600 "${WORK_DIR}/agent-card-private.pem"
	chmod 600 "${WORK_DIR}/usage-receipt-private.pem"
	"${USAGE_RECEIPT_TOOL}" public-jwk --private-key "${WORK_DIR}/usage-receipt-private.pem" \
		--key-id fgentic-org-a-usage-receipt-v1 >"${WORK_DIR}/usage-receipt-public-jwk.json"
	RECEIPT_EXTENSION='https://fgentic.fmind.ai/a2a/extensions/usage-receipt/v1' \
		RECEIPT_KEY_ID=fgentic-org-a-usage-receipt-v1 \
		RECEIPT_JWK_FILE="${WORK_DIR}/usage-receipt-public-jwk.json" yq --inplace '
  (.capabilities.extensions[] | select(.uri == strenv(RECEIPT_EXTENSION)).params.keyId) =
    strenv(RECEIPT_KEY_ID) |
  (.capabilities.extensions[] | select(.uri == strenv(RECEIPT_EXTENSION)).params.publicJwk) =
    load(strenv(RECEIPT_JWK_FILE))
' "${WORK_DIR}/unsigned-agent-card.json"
	"${AGENT_CARD_SIGNER}" sign --input "${WORK_DIR}/unsigned-agent-card.json" \
		--private-key "${WORK_DIR}/agent-card-private.pem" \
		--key-id fgentic-org-a-docs-qa-v1 --output "${WORK_DIR}/agent-card-bundle.json"
	jq --join-output --compact-output '.agentCard' "${WORK_DIR}/agent-card-bundle.json" \
		>"${WORK_DIR}/signed-agent-card.json"
	jq --join-output --compact-output '.publicJwk' "${WORK_DIR}/agent-card-bundle.json" \
		>"${WORK_DIR}/agent-card-public-jwk.json"
	"${AGENT_CARD_SIGNER}" verify --input "${WORK_DIR}/signed-agent-card.json" \
		--public-key "${WORK_DIR}/agent-card-public-jwk.json" \
		--key-id fgentic-org-a-docs-qa-v1
	jq -e '
  (.agentCard.signatures | length) == 1 and
  (.agentCard.signatures[0].header == null) and
  .agentCard.securityRequirements[0].schemes.orgBOIDC == {"list": []} and
  .publicJwk.kty == "EC" and .publicJwk.crv == "P-256" and
  .publicJwk.alg == "ES256" and .publicJwk.use == "sig" and
  .publicJwk.key_ops == ["verify"] and (.publicJwk | has("d") | not)
' "${WORK_DIR}/agent-card-bundle.json" >/dev/null \
		|| fail 'AgentCard signer did not emit the exact public ES256 contract'
	jq -e --slurpfile receipt_jwk "${WORK_DIR}/usage-receipt-public-jwk.json" '
  any(.capabilities.extensions[]?;
    .uri == "https://fgentic.fmind.ai/a2a/extensions/usage-receipt/v1" and
    .required == true and .params.schema == "fgentic.usage-receipt.v1" and
    .params.keyId == "fgentic-org-a-usage-receipt-v1" and
    .params.publicJwk == $receipt_jwk[0])
' "${WORK_DIR}/signed-agent-card.json" >/dev/null \
		|| fail 'signed AgentCard does not pin the independent usage-receipt verifier'
	# The verified card exposes the per-skill quote inside the signature (#142): a re-sign after a quote
	# change therefore yields a new signature atomically, and the price is tamper-evident for free.
	jq -e '
  .capabilities.extensions
  | map(select(.uri == "https://fgentic.fmind.ai/a2a/extensions/skill-quote/v1")) as $quoteExts
  | ($quoteExts | length) >= 1
  and all($quoteExts[];
    (.params.quotes | length) >= 1
    and all(.params.quotes[];
      (.price | type) == "number" and .price >= 0 and .price == (.price | floor)))
' "${WORK_DIR}/signed-agent-card.json" >/dev/null \
		|| fail 'signed AgentCard does not expose a well-formed per-skill quote inside its verified signature'
	protected="$(jq -er '.agentCard.signatures[0].protected' \
		"${WORK_DIR}/agent-card-bundle.json" | tr '_-' '/+')"
	case "$((${#protected} % 4))" in
		0) ;;
		2) protected="${protected}==" ;;
		3) protected="${protected}=" ;;
		*) fail 'AgentCard protected header has invalid base64url length' ;;
	esac
	printf '%s' "${protected}" | base64 --decode >"${WORK_DIR}/protected-header.json"
	jq -e '
  keys == ["alg", "kid", "typ"] and
  .alg == "ES256" and .kid == "fgentic-org-a-docs-qa-v1" and .typ == "JOSE"
' "${WORK_DIR}/protected-header.json" >/dev/null \
		|| fail 'AgentCard JWS identity fields are not all protected'
	jq '.description = "signature-tamper-must-not-be-logged"' \
		"${WORK_DIR}/signed-agent-card.json" >"${WORK_DIR}/tampered-agent-card.json"
	if "${AGENT_CARD_SIGNER}" verify --input "${WORK_DIR}/tampered-agent-card.json" \
		--public-key "${WORK_DIR}/agent-card-public-jwk.json" \
		--key-id fgentic-org-a-docs-qa-v1 \
		>"${WORK_DIR}/tamper-output.txt" 2>&1; then
		fail 'AgentCard verifier accepted a tampered signed payload'
	fi
	if rg --fixed-strings 'signature-tamper-must-not-be-logged' \
		"${WORK_DIR}/tamper-output.txt" >/dev/null; then
		fail 'AgentCard verifier logged tampered card content'
	fi

	# Zero-downtime AgentCard key-rotation rehearsal (#352 Tasks 2/5), fully offline. Sign the SAME prepared
	# card under an overlap (`-next`) and a retired (`-prev`) kid (the primary was already signed above), then
	# drive the merged bridge verifier (agentcardjws.VerifySet, #920/#939) through the exact outcomes the
	# fed:up acceptance (verify_agent_card_rotation) asserts: mid-overlap both pinned kids verify, the retired
	# kid is refused fail-closed once revoked while the promoted kid still verifies, and a tampered card is
	# still refused — all content-free. This de-risks the runtime proof without a cluster.
	local rotation_primary=fgentic-org-a-docs-qa-v1
	local rotation_overlap="${rotation_primary}-next"
	local rotation_revoked="${rotation_primary}-prev"
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "${WORK_DIR}/rotation-overlap-private.pem" 2>/dev/null
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
		-out "${WORK_DIR}/rotation-revoked-private.pem" 2>/dev/null
	chmod 600 "${WORK_DIR}/rotation-overlap-private.pem" "${WORK_DIR}/rotation-revoked-private.pem"
	"${AGENT_CARD_SIGNER}" sign --input "${WORK_DIR}/unsigned-agent-card.json" \
		--private-key "${WORK_DIR}/rotation-overlap-private.pem" --key-id "${rotation_overlap}" \
		--output "${WORK_DIR}/rotation-overlap-bundle.json"
	"${AGENT_CARD_SIGNER}" sign --input "${WORK_DIR}/unsigned-agent-card.json" \
		--private-key "${WORK_DIR}/rotation-revoked-private.pem" --key-id "${rotation_revoked}" \
		--output "${WORK_DIR}/rotation-revoked-bundle.json"
	jq --join-output --compact-output '.agentCard' "${WORK_DIR}/rotation-overlap-bundle.json" \
		>"${WORK_DIR}/rotation-overlap-card.json"
	jq --join-output --compact-output '.publicJwk' "${WORK_DIR}/rotation-overlap-bundle.json" \
		>"${WORK_DIR}/rotation-overlap-jwk.json"
	jq --join-output --compact-output '.agentCard' "${WORK_DIR}/rotation-revoked-bundle.json" \
		>"${WORK_DIR}/rotation-revoked-card.json"
	# (a) mid-overlap: the primary and overlap cards both verify against the pinned set {primary, overlap}.
	"${AGENT_CARD_SIGNER}" verify --input "${WORK_DIR}/signed-agent-card.json" \
		--public-key "${WORK_DIR}/agent-card-public-jwk.json" --key-id "${rotation_primary}" \
		--additional-key "${rotation_overlap}=${WORK_DIR}/rotation-overlap-jwk.json" \
		|| fail 'rotation rehearsal: primary card did not verify mid-overlap'
	"${AGENT_CARD_SIGNER}" verify --input "${WORK_DIR}/rotation-overlap-card.json" \
		--public-key "${WORK_DIR}/agent-card-public-jwk.json" --key-id "${rotation_primary}" \
		--additional-key "${rotation_overlap}=${WORK_DIR}/rotation-overlap-jwk.json" \
		|| fail 'rotation rehearsal: overlap card did not verify mid-overlap'
	# (b) after revocation: the retired-kid card is refused fail-closed; the promoted kid still verifies.
	if "${AGENT_CARD_SIGNER}" verify --input "${WORK_DIR}/rotation-revoked-card.json" \
		--public-key "${WORK_DIR}/agent-card-public-jwk.json" --key-id "${rotation_primary}" \
		--additional-key "${rotation_overlap}=${WORK_DIR}/rotation-overlap-jwk.json" \
		--revoked-key-id "${rotation_revoked}" \
		>"${WORK_DIR}/rotation-revoked-output.txt" 2>&1; then
		fail 'rotation rehearsal: a revoked kid was accepted'
	fi
	if rg --fixed-strings 'fgentic-documentation' "${WORK_DIR}/rotation-revoked-output.txt" >/dev/null; then
		fail 'rotation rehearsal: revoked verification logged card content'
	fi
	"${AGENT_CARD_SIGNER}" verify --input "${WORK_DIR}/rotation-overlap-card.json" \
		--public-key "${WORK_DIR}/agent-card-public-jwk.json" --key-id "${rotation_primary}" \
		--additional-key "${rotation_overlap}=${WORK_DIR}/rotation-overlap-jwk.json" \
		--revoked-key-id "${rotation_revoked}" \
		|| fail 'rotation rehearsal: promoted kid did not verify after revocation'
	# (c) an unrevoked but tampered card is still refused and never logs its mutated body.
	jq '.description = "rotation-tamper-must-not-be-logged"' "${WORK_DIR}/signed-agent-card.json" \
		>"${WORK_DIR}/rotation-tampered-card.json"
	if "${AGENT_CARD_SIGNER}" verify --input "${WORK_DIR}/rotation-tampered-card.json" \
		--public-key "${WORK_DIR}/agent-card-public-jwk.json" --key-id "${rotation_primary}" \
		--additional-key "${rotation_overlap}=${WORK_DIR}/rotation-overlap-jwk.json" \
		>"${WORK_DIR}/rotation-tamper-output.txt" 2>&1; then
		fail 'rotation rehearsal: a tampered card was accepted'
	fi
	if rg --fixed-strings 'rotation-tamper-must-not-be-logged' \
		"${WORK_DIR}/rotation-tamper-output.txt" >/dev/null; then
		fail 'rotation rehearsal: tampered verification logged card content'
	fi

	for private_suffix in pem key; do
		git -C "${ROOT_DIR}" check-ignore --quiet --no-index \
			"${ROOT_DIR}/do-not-create-agent-card-test.${private_suffix}" \
			|| fail "*.${private_suffix} private keys are not git-ignored"
	done
	if jq -e 'has("signatures")' "${AGENT_CARD_TEMPLATE}" >/dev/null; then
		fail 'tracked AgentCard template is already signed'
	fi

	# Public CA material is copied into every Matrix namespace at runtime; the repository and cluster
	# snapshots must never carry the local signing key.
	for contract in \
		'for namespace in matrix matrix-b matrix-c' \
		'create configmap fgentic-local-ca' \
		'ca.crt' \
		'pg-synapse-b' \
		'pg-synapse-c' \
		'pg-keycloak' \
		'pg-kagent' \
		'charlie-password' \
		'org-b-a2a-client-secret' \
		'untrusted-a2a-client-secret' \
		'wrong-audience-a2a-client-secret' \
		'prepare_federation_agent_card_key' \
		'refusing to rotate a missing AgentCard key while public artifacts still exist' \
		'existing federation AgentCard public JWK is invalid' \
		'refusing to replace the independently pinnable AgentCard public JWK' \
		'--patch-file /dev/stdin' \
		'sign_federation_agent_card_snapshot' \
		'federation AgentCard contains an unresolved substitution before signing' \
		'prepare_federation_agent_card_rotation' \
		'sign_agent_card_snapshot_variant' \
		'AGENT_CARD_OVERLAP_KEY_ID="${AGENT_CARD_KEY_ID}-next"' \
		'AGENT_CARD_REVOKED_KEY_ID="${AGENT_CARD_KEY_ID}-prev"' \
		'agent-card-keys.json=${AGENT_CARD_ROTATION_KEYS_FILE}' \
		'publish_federation_agent_card_artifacts' \
		'agent-card-private-key=${AGENT_CARD_PRIVATE_KEY}' \
		'agent-card.json=${AGENT_CARD_PUBLIC_FILE}' \
		'public-jwk.json=${AGENT_CARD_JWK_FILE}' \
		'usage-receipt-private-key=${USAGE_RECEIPT_PRIVATE_KEY}' \
		'usage-receipt-public-jwk.json=${USAGE_RECEIPT_JWK_FILE}' \
		'apply_secret agentgateway-system federated-usage-receipt-signing' \
		'apply_secret postgres pg-synapse-c' \
		'--from-literal=username=synapse_c' \
		'apply_secret matrix-c pg-synapse-c'; do
		rg --fixed-strings -- "${contract}" "${DEMO_SOURCES[@]}" >/dev/null \
			|| fail "federation lifecycle omits ${contract}"
	done
	if rg --regexp 'apply_secret[[:space:]]+agentgateway-system.*agent-card-private' \
		"${DEMO_SOURCES[@]}" >/dev/null; then
		fail 'AgentCard private key is published into the runtime gateway namespace'
	fi
	if rg --regexp='--arg.*encoded_private_key' "${DEMO_SOURCES[@]}" >/dev/null; then
		fail 'AgentCard private key is exposed through a process argument'
	fi
	if rg --fixed-strings 'ca.key' "${LIFECYCLE}" "${DEMO_SOURCES[@]}" \
		"${CLUSTER_OVERLAY}" "${FEDERATION_ROOT}" >/dev/null; then
		fail 'federation assets reference the private local-CA key'
	fi

	# A retained pre-receipt lab has an AgentCard key and public ConfigMap, but no receipt key or JWK.
	# Upgrading that exact state must add the new independent key rather than treating a missing
	# ConfigMap data entry as the literal go-template string "<no value>".
	legacy_private_key="$(base64 <"${WORK_DIR}/agent-card-private.pem" | tr -d '\n')"
	legacy_key_id="$(printf '%s' fgentic-org-a-docs-qa-v1 | base64 | tr -d '\n')"
	legacy_public_jwk="$(<"${WORK_DIR}/agent-card-public-jwk.json")"
	legacy_bootstrap="$(jq --null-input --arg private_key "${legacy_private_key}" \
		--arg key_id "${legacy_key_id}" '{
    data: {
      "agent-card-private-key": $private_key,
      "agent-card-key-id": $key_id
    }
  }')"
	legacy_configmap="$(jq --null-input --arg public_jwk "${legacy_public_jwk}" '{
    data: {"public-jwk.json": $public_jwk}
  }')"
	legacy_patch="${WORK_DIR}/legacy-receipt-bootstrap-patch"
	: >"${legacy_patch}"
	(
		# shellcheck source=scripts/lib/demo-federation.sh
		source "${ROOT_DIR}/scripts/lib/demo-federation.sh"
		die() {
			echo "error: $*" >&2
			exit 1
		}
		apply_secret() {
			die "legacy bootstrap unexpectedly replaced its existing Secret"
		}
		kubectl() {
			case "$*" in
				'create namespace flux-system --dry-run=client --output=yaml')
					printf '%s\n' 'apiVersion: v1' 'kind: Namespace'
					;;
				'apply --filename -')
					cat >/dev/null
					;;
				*'get secret fgentic-demo-bootstrap --output json'*)
					printf '%s\n' "${legacy_bootstrap}"
					;;
				*'get configmap fgentic-agent-card --output json'*)
					printf '%s\n' "${legacy_configmap}"
					;;
				*'get secret fgentic-demo-bootstrap')
					return 0
					;;
				*'create secret generic fgentic-demo-bootstrap '*'--output=json'*)
					printf '%s\n' '{"data":{"usage-receipt-private-key":"fixture"}}'
					;;
				*'patch secret fgentic-demo-bootstrap '*)
					cat >/dev/null
					printf '%s\n' patched >"${legacy_patch}"
					;;
				*) die "unexpected legacy bootstrap kubectl call: $*" ;;
			esac
		}
		FEDERATION_AGENT_CARD_CONFIGMAP=fgentic-agent-card
		FEDERATION_AGENT_CARD_DEFAULT_KEY_ID=fgentic-org-a-docs-qa-v1
		FEDERATION_USAGE_RECEIPT_DEFAULT_KEY_ID=fgentic-org-a-usage-receipt-v1
		prepare_federation_agent_card_key
		[ -z "${EXISTING_USAGE_RECEIPT_JWK_FILE}" ] \
			|| die "legacy ConfigMap exposed a nonexistent usage-receipt JWK"
		[ "${USAGE_RECEIPT_KEY_ID}" = "${FEDERATION_USAGE_RECEIPT_DEFAULT_KEY_ID}" ] \
			|| die "legacy upgrade generated the wrong usage-receipt key ID"
		[ -s "${USAGE_RECEIPT_PRIVATE_KEY}" ] \
			|| die "legacy upgrade did not generate a usage-receipt private key"
	)
	[ "$(<"${legacy_patch}")" = patched ] \
		|| fail 'legacy federation bootstrap did not persist the new usage-receipt key'

}
