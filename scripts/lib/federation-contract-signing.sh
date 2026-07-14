#!/usr/bin/env bash
# Definition-only federation signing contracts sourced by scripts/test-federation.sh.
check_federation_signing() {
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
chmod 600 "${WORK_DIR}/agent-card-private.pem"
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
  .publicJwk.kty == "EC" and .publicJwk.crv == "P-256" and
  .publicJwk.alg == "ES256" and .publicJwk.use == "sig" and
  .publicJwk.key_ops == ["verify"] and (.publicJwk | has("d") | not)
' "${WORK_DIR}/agent-card-bundle.json" >/dev/null ||
	fail 'AgentCard signer did not emit the exact public ES256 contract'
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
' "${WORK_DIR}/signed-agent-card.json" >/dev/null ||
	fail 'signed AgentCard does not expose a well-formed per-skill quote inside its verified signature'
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
' "${WORK_DIR}/protected-header.json" >/dev/null ||
	fail 'AgentCard JWS identity fields are not all protected'
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
for private_suffix in pem key; do
	git -C "${ROOT_DIR}" check-ignore --quiet --no-index \
		"${ROOT_DIR}/do-not-create-agent-card-test.${private_suffix}" ||
		fail "*.${private_suffix} private keys are not git-ignored"
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
		'publish_federation_agent_card_artifacts' \
		'agent-card-private-key=${AGENT_CARD_PRIVATE_KEY}' \
		'agent-card.json=${AGENT_CARD_PUBLIC_FILE}' \
		'public-jwk.json=${AGENT_CARD_JWK_FILE}' \
		'apply_secret postgres pg-synapse-c' \
	'--from-literal=username=synapse_c' \
	'apply_secret matrix-c pg-synapse-c'; do
	rg --fixed-strings -- "${contract}" "${DEMO_SOURCES[@]}" >/dev/null ||
		fail "federation lifecycle omits ${contract}"
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

}
