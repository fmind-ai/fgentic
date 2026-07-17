#!/usr/bin/env bash
# Focused offline contracts for split-control-plane federation seed routing.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly SEED="${ROOT_DIR}/scripts/seed-federation.sh"
readonly A2A_HELPERS="${ROOT_DIR}/scripts/lib/federation-a2a.sh"
readonly MATRIX_HELPERS="${ROOT_DIR}/scripts/lib/federation-matrix.sh"
readonly SIGNING_HELPERS="${ROOT_DIR}/scripts/lib/federation-signing.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-split-seed-check.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

fail() {
	echo "error: $*" >&2
	exit 1
}

assert_contains() {
	local path="$1"
	local expected="$2"
	local message="$3"
	rg --quiet --fixed-strings -- "${expected}" "${path}" || fail "${message}"
}

for command in awk bash cat chmod cp date jq mkdir mise mktemp openssl rg; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

readonly CA_A_DIR="${WORK_DIR}/ca-a"
readonly CA_B_DIR="${WORK_DIR}/ca-b"
readonly CA_SAME_DIR="${WORK_DIR}/ca-same"
readonly HOST_CA_BUNDLE="${WORK_DIR}/host-ca-bundle.pem"
readonly SAME_CA_BUNDLE="${WORK_DIR}/same-ca-bundle.pem"
readonly PRIVATE_CA_BUNDLE="${WORK_DIR}/private-ca-bundle.pem"
readonly KUBECONFIG_A="${WORK_DIR}/a.kubeconfig"
readonly KUBECONFIG_B="${WORK_DIR}/b.kubeconfig"
readonly FAKE_BIN="${WORK_DIR}/bin"
readonly KUBECTL_LOG="${WORK_DIR}/kubectl.log"
readonly CURL_LOG="${WORK_DIR}/curl.log"
readonly MISE_LOG="${WORK_DIR}/mise.log"
readonly SIGNING_INPUT_LOG="${WORK_DIR}/signing-input.jsonl"
readonly CURL_BODY_CHECK_LOG="${WORK_DIR}/curl-body-check.log"
REAL_MISE="$(command -v mise)"
readonly REAL_MISE

mkdir -p "${CA_A_DIR}" "${CA_B_DIR}" "${CA_SAME_DIR}" "${FAKE_BIN}"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=fgentic-split-a-test' \
	-keyout "${CA_A_DIR}/ca.key" -out "${CA_A_DIR}/ca.crt" >/dev/null 2>&1
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=fgentic-split-b-test' \
	-keyout "${CA_B_DIR}/ca.key" -out "${CA_B_DIR}/ca.crt" >/dev/null 2>&1
cp "${CA_A_DIR}/ca.crt" "${CA_SAME_DIR}/ca.crt"
awk '1' "${CA_A_DIR}/ca.crt" "${CA_B_DIR}/ca.crt" >"${HOST_CA_BUNDLE}"
awk '1' "${CA_A_DIR}/ca.crt" "${CA_A_DIR}/ca.crt" >"${SAME_CA_BUNDLE}"
awk '1' "${CA_A_DIR}/ca.crt" "${CA_B_DIR}/ca.crt" "${CA_A_DIR}/ca.key" \
	>"${PRIVATE_CA_BUNDLE}"
printf 'apiVersion: v1\nkind: Config\n' >"${KUBECONFIG_A}"
printf 'apiVersion: v1\nkind: Config\n' >"${KUBECONFIG_B}"
: >"${KUBECTL_LOG}"
: >"${CURL_LOG}"
: >"${MISE_LOG}"
: >"${SIGNING_INPUT_LOG}"
: >"${CURL_BODY_CHECK_LOG}"

cat >"${FAKE_BIN}/kubectl" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
{
	printf 'kubectl'
	printf ' <%s>' "$@"
	printf '\n'
} >>"${FAKE_KUBECTL_LOG:?}"

kubeconfig=""
if [ "${1:-}" = --kubeconfig ]; then
	kubeconfig="${2:-}"
	shift 2
fi

case " $* " in
*" --namespace kube-system get namespace kube-system --output jsonpath={.metadata.uid} "*)
	case "${kubeconfig}" in
	"${EXPECTED_A_KUBECONFIG:?}") printf '%s' "${FAKE_A_UID:-uid-control-plane-a}" ;;
	"${EXPECTED_B_KUBECONFIG:?}") printf '%s' "${FAKE_B_UID:-uid-control-plane-b}" ;;
	*) exit 41 ;;
	esac
	;;
*" get namespaces --output json "*)
	case "${kubeconfig}" in
	"${EXPECTED_A_KUBECONFIG:?}")
		if [ "${FAKE_SPLIT_INVENTORY_CONTAMINATED:-no}" = yes ]; then
			printf '%s' '{"items":[{"metadata":{"name":"matrix"}},{"metadata":{"name":"matrix-c"}},{"metadata":{"name":"agentgateway-system"}},{"metadata":{"name":"kagent"}},{"metadata":{"name":"matrix-b"}}]}'
		else
			printf '%s' '{"items":[{"metadata":{"name":"matrix"}},{"metadata":{"name":"matrix-c"}},{"metadata":{"name":"agentgateway-system"}},{"metadata":{"name":"kagent"}}]}'
		fi
		;;
	"${EXPECTED_B_KUBECONFIG:?}")
		printf '%s' '{"items":[{"metadata":{"name":"matrix-b"}},{"metadata":{"name":"keycloak"}}]}'
		;;
	*) exit 44 ;;
	esac
	;;
*" get pods --all-namespaces --output json "*)
	case "${kubeconfig}" in
	"${EXPECTED_A_KUBECONFIG:?}")
		if [ "${FAKE_SPLIT_INVENTORY_CONTAMINATED:-no}" = yes ]; then
			printf '%s' '{"items":[{"metadata":{"namespace":"matrix"}},{"metadata":{"namespace":"matrix-c"}},{"metadata":{"namespace":"agentgateway-system"}},{"metadata":{"namespace":"kagent"}},{"metadata":{"namespace":"matrix-b"}}]}'
		else
			printf '%s' '{"items":[{"metadata":{"namespace":"matrix"}},{"metadata":{"namespace":"matrix-c"}},{"metadata":{"namespace":"agentgateway-system"}},{"metadata":{"namespace":"kagent"}}]}'
		fi
		;;
	"${EXPECTED_B_KUBECONFIG:?}")
		printf '%s' '{"items":[{"metadata":{"namespace":"matrix-b"}},{"metadata":{"namespace":"keycloak"}}]}'
		;;
	*) exit 45 ;;
	esac
	;;
*" --namespace matrix-c exec --stdin statefulset/ess-synapse-main "*)
	signing_input="$(cat)"
	printf '%s\n' "${signing_input}" >>"${FAKE_SIGNING_INPUT_LOG:?}"
	printf 'ed25519:test\tc2lnbmF0dXJl\n'
	;;
*" get secret ess-generated "*)
	case " $* " in
	*" --namespace matrix "*) printf '%s' "${FAKE_REGISTRATION_SECRET_A:?}" ;;
	*" --namespace matrix-b "*) printf '%s' "${FAKE_REGISTRATION_SECRET_B:?}" ;;
	*" --namespace matrix-c "*) printf '%s' "${FAKE_REGISTRATION_SECRET_C:?}" ;;
	*) exit 43 ;;
	esac
	;;
*" get secret fgentic-demo-bootstrap "*)
	case "$*" in
	*alice-password*) printf '%s' "${FAKE_ALICE_SECRET:?}" ;;
	*bob-password*) printf '%s' "${FAKE_BOB_SECRET:?}" ;;
	*charlie-password*) printf '%s' "${FAKE_CHARLIE_SECRET:?}" ;;
	*org-b-a2a-client-secret*) printf '%s' "${FAKE_ORG_B_SECRET:?}" ;;
	*untrusted-a2a-client-secret*) printf '%s' "${FAKE_UNTRUSTED_SECRET:?}" ;;
	*wrong-audience-a2a-client-secret*) printf '%s' "${FAKE_WRONG_AUDIENCE_SECRET:?}" ;;
	*) exit 42 ;;
	esac
	;;
*) printf 'ok' ;;
esac
FAKE_KUBECTL
chmod +x "${FAKE_BIN}/kubectl"

cat >"${FAKE_BIN}/mise" <<'FAKE_MISE'
#!/usr/bin/env bash
set -euo pipefail
{
	printf 'mise'
	printf ' <%s>' "$@"
	printf '\n'
} >>"${FAKE_MISE_LOG:?}"
exec "${REAL_MISE:?}" "$@"
FAKE_MISE
chmod +x "${FAKE_BIN}/mise"

cat >"${FAKE_BIN}/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail
{
	printf 'curl'
	printf ' <%s>' "$@"
	printf '\n'
} >>"${FAKE_CURL_LOG:?}"

output=""
write_out=""
method="GET"
body_from_stdin="no"
inline_body=""
url=""
while (($# > 0)); do
	case "$1" in
	--output | --write-out | --request | --data | --data-binary | --header | --cacert | --resolve | --noproxy)
		option="$1"
		value="${2:-}"
		case "${option}" in
		--output) output="${value}" ;;
		--write-out) write_out="${value}" ;;
		--request) method="${value}" ;;
		--data) inline_body="${value}" ;;
		--data-binary)
			[ "${value}" != @- ] || body_from_stdin="yes"
			;;
		esac
		shift 2
		;;
	--silent | --show-error | --fail-with-body)
		shift
		;;
	*)
		url="$1"
		shift
		;;
	esac
done

body=""
[ "${body_from_stdin}" != yes ] || body="$(cat)"

respond() {
	local status="$1"
	local response="$2"
	if [ -n "${output}" ]; then
		printf '%s' "${response}" >"${output}"
	else
		printf '%s' "${response}"
	fi
	[ -z "${write_out}" ] || printf '%s' "${status}"
}

case "${url}" in
*/_synapse/admin/v1/register)
	if [ "${method}" = POST ]; then
		[ "${body_from_stdin}" = yes ] || exit 51
		username="$(jq -er '.username' <<<"${body}")"
		password="$(jq -er '.password' <<<"${body}")"
		mac="$(jq -er '.mac' <<<"${body}")"
		case "${username}" in
		alice) expected_password="${FAKE_ALICE_SECRET:?}" ;;
		bob) expected_password="${FAKE_BOB_SECRET:?}" ;;
		charlie) expected_password="${FAKE_CHARLIE_SECRET:?}" ;;
		*) exit 52 ;;
		esac
		[ "${password}" = "${expected_password}" ] || exit 53
		[[ "${mac}" =~ ^[0-9a-f]{40}$ ]] || exit 54
		printf 'register:%s\n' "${username}" >>"${FAKE_CURL_BODY_CHECK_LOG:?}"
		respond 200 '{}'
	else
		respond 200 '{"nonce":"fake-registration-nonce"}'
	fi
	;;
*/_matrix/client/v3/login)
	[ "${body_from_stdin}" = yes ] || exit 55
	username="$(jq -er '.identifier.user' <<<"${body}")"
	password="$(jq -er '.password' <<<"${body}")"
	case "${username}" in
	alice) expected_password="${FAKE_ALICE_SECRET:?}" ;;
	bob) expected_password="${FAKE_BOB_SECRET:?}" ;;
	charlie) expected_password="${FAKE_CHARLIE_SECRET:?}" ;;
	*) exit 56 ;;
	esac
	[ "${password}" = "${expected_password}" ] || exit 57
	printf 'login:%s\n' "${username}" >>"${FAKE_CURL_BODY_CHECK_LOG:?}"
	respond 200 "{\"access_token\":\"token-${username}\"}"
	;;
*/realms/fgentic-federation/protocol/openid-connect/token)
	[ "${body_from_stdin}" = yes ] || exit 58
	[[ "${body}" == *"client_secret=${FAKE_ORG_B_SECRET:?}"* ]] || exit 59
	printf 'client-credentials:org-b-a2a\n' >>"${FAKE_CURL_BODY_CHECK_LOG:?}"
	respond 200 '{"access_token":"fake-token"}'
	;;
*/_matrix/federation/v1/send/*)
	[[ "${inline_body}" == *'"destination"'* ]] || exit 60
	respond 403 '{"errcode":"M_FORBIDDEN","error":"denied"}'
	;;
*/route-map-probe)
	respond 200 '{}'
	;;
*) respond 200 '{}' ;;
esac
FAKE_CURL
chmod +x "${FAKE_BIN}/curl"

export FAKE_KUBECTL_LOG="${KUBECTL_LOG}"
export FAKE_CURL_LOG="${CURL_LOG}"
export FAKE_MISE_LOG="${MISE_LOG}"
export FAKE_SIGNING_INPUT_LOG="${SIGNING_INPUT_LOG}"
export FAKE_CURL_BODY_CHECK_LOG="${CURL_BODY_CHECK_LOG}"
export REAL_MISE
export EXPECTED_A_KUBECONFIG="${KUBECONFIG_A}"
export EXPECTED_B_KUBECONFIG="${KUBECONFIG_B}"
export FAKE_ALICE_SECRET='a11ce001'
export FAKE_BOB_SECRET='b0b00002'
export FAKE_CHARLIE_SECRET='c4a411e3'
export FAKE_ORG_B_SECRET='0b0a2a01'
export FAKE_UNTRUSTED_SECRET='bad00002'
export FAKE_WRONG_AUDIENCE_SECRET='bad00003'
export FAKE_REGISTRATION_SECRET_A='registration-a-secret'
export FAKE_REGISTRATION_SECRET_B='registration-b-secret'
export FAKE_REGISTRATION_SECRET_C='registration-c-secret'

split_validation() {
	local -a overrides=("$@")
	env PATH="${FAKE_BIN}:${PATH}" \
		FAKE_KUBECTL_LOG="${KUBECTL_LOG}" \
		EXPECTED_A_KUBECONFIG="${KUBECONFIG_A}" \
		EXPECTED_B_KUBECONFIG="${KUBECONFIG_B}" \
		FGENTIC_FED_LAYOUT=split \
		FGENTIC_FED_A_KUBECONFIG="${KUBECONFIG_A}" \
		FGENTIC_FED_B_KUBECONFIG="${KUBECONFIG_B}" \
		FGENTIC_FED_CA_DIR_A="${CA_A_DIR}" \
		FGENTIC_FED_CA_DIR_B="${CA_B_DIR}" \
		FGENTIC_FED_HOST_CA_BUNDLE="${HOST_CA_BUNDLE}" \
		"${overrides[@]}" \
		bash -c 'source "$1"; validate_federation_layout' _ "${SEED}"
}

split_inventory_validation() {
	env PATH="${FAKE_BIN}:${PATH}" \
		FAKE_KUBECTL_LOG="${KUBECTL_LOG}" \
		EXPECTED_A_KUBECONFIG="${KUBECONFIG_A}" \
		EXPECTED_B_KUBECONFIG="${KUBECONFIG_B}" \
		FAKE_SPLIT_INVENTORY_CONTAMINATED=yes \
		FGENTIC_FED_LAYOUT=split \
		FGENTIC_FED_A_KUBECONFIG="${KUBECONFIG_A}" \
		FGENTIC_FED_B_KUBECONFIG="${KUBECONFIG_B}" \
		FGENTIC_FED_CA_DIR_A="${CA_A_DIR}" \
		FGENTIC_FED_CA_DIR_B="${CA_B_DIR}" \
		FGENTIC_FED_HOST_CA_BUNDLE="${HOST_CA_BUNDLE}" \
		bash -c 'source "$1"; validate_federation_layout; verify_split_runtime_inventory' \
		_ "${SEED}"
}

expect_validation_failure() {
	local label="$1"
	shift
	if "$@" >"${WORK_DIR}/failure.out" 2>&1; then
		fail "split validation accepted ${label}"
	fi
}

# The selector is internal and canonical remains the exact no-selector default: localhost names,
# one 127.0.0.2 ingress, the existing CA location, and ambient kubectl invocation.
readonly CANONICAL_CA_DIR="${WORK_DIR}/canonical-ca"
mkdir -p "${CANONICAL_CA_DIR}"
printf 'canonical fixture\n' >"${CANONICAL_CA_DIR}/ca.crt"
: >"${KUBECTL_LOG}"
env -u FGENTIC_FED_LAYOUT -u FGENTIC_FED_A_KUBECONFIG -u FGENTIC_FED_B_KUBECONFIG \
	-u FGENTIC_FED_CA_DIR_A -u FGENTIC_FED_CA_DIR_B -u FGENTIC_FED_HOST_CA_BUNDLE \
	PATH="${FAKE_BIN}:${PATH}" FGENTIC_CA_DIR="${CANONICAL_CA_DIR}" \
	FAKE_KUBECTL_LOG="${KUBECTL_LOG}" \
	bash -c '
		source "$1"
		[ "${FGENTIC_FED_LAYOUT}" = canonical ]
		[ "${SERVER_A}" = org-a.fgentic.localhost ]
		[ "${SERVER_B}" = org-b.fgentic.localhost ]
		[ "${SERVER_C}" = org-c.fgentic.localhost ]
		[ "${FEDERATION_LOOPBACK_A}" = 127.0.0.2 ]
		[ "${FEDERATION_LOOPBACK_B}" = 127.0.0.2 ]
		validate_federation_layout
		federation_kubectl A get canonical-a >/dev/null
		federation_kubectl B get canonical-b >/dev/null
	' _ "${SEED}"
if rg --quiet --fixed-strings -- '--kubeconfig' "${KUBECTL_LOG}"; then
	fail 'canonical seed unexpectedly supplied a split kubeconfig'
fi
assert_contains "${KUBECTL_LOG}" '<get> <canonical-a>' \
	'canonical control-plane A did not preserve ambient kubectl behavior'
assert_contains "${KUBECTL_LOG}" '<get> <canonical-b>' \
	'canonical logical control-plane B did not preserve the one-cluster behavior'

# Split mode requires two real, distinct API identities before any proof operation, uses the
# two-root public host bundle, and routes namespaces/secrets through explicit kubeconfigs.
: >"${KUBECTL_LOG}"
PATH="${FAKE_BIN}:${PATH}" \
	KUBECONFIG="${WORK_DIR}/ambient-kubeconfig-must-not-be-used" \
	FGENTIC_FED_LAYOUT=split \
	FGENTIC_FED_A_KUBECONFIG="${KUBECONFIG_A}" \
	FGENTIC_FED_B_KUBECONFIG="${KUBECONFIG_B}" \
	FGENTIC_FED_CA_DIR_A="${CA_A_DIR}" \
	FGENTIC_FED_CA_DIR_B="${CA_B_DIR}" \
	FGENTIC_FED_HOST_CA_BUNDLE="${HOST_CA_BUNDLE}" \
	FAKE_CURL_LOG="${CURL_LOG}" \
	bash -c '
		source "$1"
		validate_federation_layout
		verify_split_runtime_inventory
		[ "${SERVER_A}" = org-a.fgentic.test ]
		[ "${SERVER_B}" = org-b.fgentic.test ]
		[ "${SERVER_C}" = org-c.fgentic.test ]
		[ "${FEDERATION_LOOPBACK_A}" = 127.0.0.2 ]
		[ "${FEDERATION_LOOPBACK_B}" = 127.0.0.3 ]
		[ "${CONTROL_PLANE_A_UID}" = uid-control-plane-a ]
		[ "${CONTROL_PLANE_B_UID}" = uid-control-plane-b ]
		federation_kubectl A get a-runtime >/dev/null
		federation_kubectl B get b-runtime >/dev/null
		federation_matrix_kubectl matrix get alice-runtime >/dev/null
		federation_matrix_kubectl matrix-b get bob-runtime >/dev/null
		federation_matrix_kubectl matrix-c get charlie-runtime >/dev/null
		[ "$(federation_secret_value A alice-password)" = "${FAKE_ALICE_SECRET}" ]
		[ "$(federation_secret_value B bob-password)" = "${FAKE_BOB_SECRET}" ]
		[ "$(federation_secret_value A charlie-password)" = "${FAKE_CHARLIE_SECRET}" ]
		for key in org-b-a2a-client-secret untrusted-a2a-client-secret \
			wrong-audience-a2a-client-secret; do
			federation_secret_value B "${key}" >/dev/null
		done
		curl --silent "${MATRIX_A_URL}/route-map-probe" >/dev/null
		register_user matrix "${MATRIX_A_URL}" alice "Alice Federation" \
			"${FAKE_ALICE_SECRET}"
		register_user matrix-b "${MATRIX_B_URL}" bob "Bob Federation" \
			"${FAKE_BOB_SECRET}"
		register_user matrix-c "${MATRIX_C_URL}" charlie "Charlie Federation" \
			"${FAKE_CHARLIE_SECRET}"
		alice_login_token=""
		bob_login_token=""
		charlie_login_token=""
		login_user "${MATRIX_A_URL}" alice "${FAKE_ALICE_SECRET}" alice_login_token
		login_user "${MATRIX_B_URL}" bob "${FAKE_BOB_SECRET}" bob_login_token
		login_user "${MATRIX_C_URL}" charlie "${FAKE_CHARLIE_SECRET}" \
			charlie_login_token
		[ "${alice_login_token}" = token-alice ]
		[ "${bob_login_token}" = token-bob ]
		[ "${charlie_login_token}" = token-charlie ]
		split_token=""
		client_credentials_token org-b-a2a "${FAKE_ORG_B_SECRET}" split_token
		[ "${split_token}" = fake-token ]
		authorization=""
		sign_federation_request "${SERVER_A}" /_matrix/federation/v1/send/direct-sign \
			"{\"destination\":\"${SERVER_A}\",\"pdus\":[],\"edus\":[]}" authorization
		[[ "${authorization}" == *"destination=\"${SERVER_A}\""* ]]
		status_a=""
		status_b=""
		send_signed_federation_probe "${SERVER_A}" "${MATRIX_A_URL}" '!room:test' \
			"${WORK_DIR}/signed-a.json" status_a
		send_signed_federation_probe "${SERVER_B}" "${MATRIX_B_URL}" '!room:test' \
			"${WORK_DIR}/signed-b.json" status_b
		[ "${status_a}" = 403 ]
		[ "${status_b}" = 403 ]
	' _ "${SEED}"

if rg --quiet --fixed-strings 'ambient-kubeconfig-must-not-be-used' "${KUBECTL_LOG}"; then
	fail 'split seed fell back to ambient KUBECONFIG'
fi

route_line="$(rg --no-config --max-columns=0 --fixed-strings -- \
	'<https://matrix.org-a.fgentic.test/route-map-probe>' "${CURL_LOG}")"
actual_resolve_map="$(rg --only-matching -- '<--resolve> <[^>]+>' <<<"${route_line}")"
expected_resolve_map="$(printf '%s\n' \
	'<--resolve> <org-a.fgentic.test:443:127.0.0.2>' \
	'<--resolve> <matrix.org-a.fgentic.test:443:127.0.0.2>' \
	'<--resolve> <org-b.fgentic.test:443:127.0.0.3>' \
	'<--resolve> <matrix.org-b.fgentic.test:443:127.0.0.3>' \
	'<--resolve> <org-c.fgentic.test:443:127.0.0.2>' \
	'<--resolve> <matrix.org-c.fgentic.test:443:127.0.0.2>' \
	'<--resolve> <a2a.org-a.fgentic.test:443:127.0.0.2>' \
	'<--resolve> <id.org-b.fgentic.test:443:127.0.0.3>')"
if [ "${actual_resolve_map}" != "${expected_resolve_map}" ]; then
	printf 'expected resolve map:\n%s\nactual resolve map:\n%s\n' \
		"${expected_resolve_map}" "${actual_resolve_map}" >&2
	fail 'split curl wrapper did not emit the exact A/B/C host resolve map'
fi
[[ "${route_line}" == 'curl <--noproxy> <*> '* ]] ||
	fail 'split curl wrapper did not disable proxies for the pinned host map'
[[ "${route_line}" == *'<--silent> <https://matrix.org-a.fgentic.test/route-map-probe>' ]] ||
	fail 'split curl wrapper route-map probe did not reach the requested Matrix A URL'

signer_lines="$(rg --fixed-strings -- \
	'<--namespace> <matrix-c> <exec> <--stdin> <statefulset/ess-synapse-main>' \
	"${KUBECTL_LOG}")"
[[ "${signer_lines}" == *"<--kubeconfig> <${KUBECONFIG_A}>"* ]] ||
	fail 'C signer did not execute through control plane A kubeconfig'
if [[ "${signer_lines}" == *"<--kubeconfig> <${KUBECONFIG_B}>"* ]]; then
	fail 'C signer executed through control plane B kubeconfig'
fi
jq --slurp --exit-status \
	--arg a org-a.fgentic.test --arg b org-b.fgentic.test --arg c org-c.fgentic.test '
    length == 3 and
    all(.[]; .origin == $c and (.destination == $a or .destination == $b)) and
    ([.[] | select(.destination == $a)] | length) == 2 and
    ([.[] | select(.destination == $b)] | length) == 1
  ' "${SIGNING_INPUT_LOG}" >/dev/null ||
	fail 'signed probes did not bind C origin to the exact A/B destinations'

for target in A B; do
	case "${target}" in
	A)
		target_server=org-a.fgentic.test
		target_url=matrix.org-a.fgentic.test
		;;
	B)
		target_server=org-b.fgentic.test
		target_url=matrix.org-b.fgentic.test
		;;
	esac
	probe_line="$(rg --no-config --max-columns=0 --fixed-strings -- \
		"<https://${target_url}/_matrix/federation/v1/send/" "${CURL_LOG}")"
	[[ "${probe_line}" == *"<--data> <{\"destination\":\"${target_server}\""* ]] ||
		fail "signed probe ${target} body did not bind the exact destination"
	[[ "${probe_line}" == *"origin=\"org-c.fgentic.test\",destination=\"${target_server}\""* ]] ||
		fail "signed probe ${target} authorization did not bind the exact destination"
done

for expected_route in \
	"<--kubeconfig> <${KUBECONFIG_A}> <--namespace> <kube-system>" \
	"<--kubeconfig> <${KUBECONFIG_B}> <--namespace> <kube-system>" \
	"<--kubeconfig> <${KUBECONFIG_A}> <get> <namespaces> <--output> <json>" \
	"<--kubeconfig> <${KUBECONFIG_A}> <get> <pods> <--all-namespaces> <--output> <json>" \
	"<--kubeconfig> <${KUBECONFIG_B}> <get> <namespaces> <--output> <json>" \
	"<--kubeconfig> <${KUBECONFIG_B}> <get> <pods> <--all-namespaces> <--output> <json>" \
	"<--kubeconfig> <${KUBECONFIG_A}> <get> <a-runtime>" \
	"<--kubeconfig> <${KUBECONFIG_B}> <get> <b-runtime>" \
	"<--kubeconfig> <${KUBECONFIG_A}> <--namespace> <matrix> <get> <alice-runtime>" \
	"<--kubeconfig> <${KUBECONFIG_B}> <--namespace> <matrix-b> <get> <bob-runtime>" \
	"<--kubeconfig> <${KUBECONFIG_A}> <--namespace> <matrix-c> <get> <charlie-runtime>"; do
	assert_contains "${KUBECTL_LOG}" "${expected_route}" \
		"split seed route is missing: ${expected_route}"
done
for key in bob-password org-b-a2a-client-secret untrusted-a2a-client-secret \
	wrong-audience-a2a-client-secret; do
	line="$(rg --fixed-strings -- "${key}" "${KUBECTL_LOG}" || true)"
	[[ "${line}" == *"<--kubeconfig> <${KUBECONFIG_B}>"* ]] ||
		fail "split secret ${key} was not read from control plane B"
done
for body_check in register:alice register:bob register:charlie \
	login:alice login:bob login:charlie client-credentials:org-b-a2a; do
	assert_contains "${CURL_BODY_CHECK_LOG}" "${body_check}" \
		"stdin-backed authentication body was not exercised: ${body_check}"
done
for secret in "${FAKE_ALICE_SECRET}" "${FAKE_BOB_SECRET}" "${FAKE_CHARLIE_SECRET}" \
	"${FAKE_ORG_B_SECRET}" "${FAKE_UNTRUSTED_SECRET}" "${FAKE_WRONG_AUDIENCE_SECRET}" \
	"${FAKE_REGISTRATION_SECRET_A}" "${FAKE_REGISTRATION_SECRET_B}" \
	"${FAKE_REGISTRATION_SECRET_C}"; do
	if rg --quiet --fixed-strings -- "${secret}" \
		"${KUBECTL_LOG}" "${CURL_LOG}" "${MISE_LOG}"; then
		fail 'split seed exposed a bootstrap password or client/registration secret in argv logs'
	fi
done

# Invalid or ambiguous split inputs fail before the acceptance proof can touch a workload.
expect_validation_failure 'an unsupported layout selector' split_validation \
	FGENTIC_FED_LAYOUT=hybrid
expect_validation_failure 'a missing A kubeconfig' split_validation \
	FGENTIC_FED_A_KUBECONFIG=
expect_validation_failure 'a relative B kubeconfig' split_validation \
	FGENTIC_FED_B_KUBECONFIG=relative.kubeconfig
expect_validation_failure 'one kubeconfig path for both control planes' split_validation \
	FGENTIC_FED_B_KUBECONFIG="${KUBECONFIG_A}"
expect_validation_failure 'two kubeconfigs with one kube-system UID' split_validation \
	FAKE_B_UID=uid-control-plane-a
expect_validation_failure 'identical CA roots' split_validation \
	FGENTIC_FED_CA_DIR_B="${CA_SAME_DIR}" \
	FGENTIC_FED_HOST_CA_BUNDLE="${SAME_CA_BUNDLE}"
expect_validation_failure 'private key material in the host CA bundle' split_validation \
	FGENTIC_FED_HOST_CA_BUNDLE="${PRIVATE_CA_BUNDLE}"
expect_validation_failure 'a cross-contaminated split runtime inventory' \
	split_inventory_validation

# Keep the high-level call sites auditable: all A workloads and C signing/logs are pinned to A,
# while Bob, B's policy mount, and all three Keycloak client secrets are pinned to B.
for route in \
	'federation_kubectl A --namespace agentgateway-system get pods' \
	'federation_kubectl A get --raw' \
	'federation_kubectl A --namespace kagent get services' \
	'federation_kubectl A get httproutes.gateway.networking.k8s.io'; do
	assert_contains "${A2A_HELPERS}" "${route}" "A2A helper omits control-plane-A route: ${route}"
done
for key in org-b-a2a-client-secret untrusted-a2a-client-secret \
	wrong-audience-a2a-client-secret; do
	assert_contains "${A2A_HELPERS}" "federation_secret_value B ${key}" \
		"Keycloak client secret ${key} is not pinned to control plane B"
done
assert_contains "${SEED}" 'alice_password="$(federation_secret_value A alice-password)"' \
	'Alice bootstrap material is not pinned to control plane A'
assert_contains "${SEED}" 'bob_password="$(federation_secret_value B bob-password)"' \
	'Bob bootstrap material is not pinned to control plane B'
assert_contains "${SEED}" 'charlie_password="$(federation_secret_value A charlie-password)"' \
	'Charlie bootstrap material is not pinned to control plane A'
assert_contains "${MATRIX_HELPERS}" 'federation_matrix_kubectl "${namespace}"' \
	'Matrix registration or policy mount bypasses namespace routing'
assert_contains "${MATRIX_HELPERS}" 'federation_kubectl A --namespace matrix logs' \
	'homeserver A policy logs are not pinned to control plane A'
assert_contains "${MATRIX_HELPERS}" "mise exec -- python -c '" \
	'Matrix bootstrap documents are not built through the repo-pinned Python boundary'
assert_contains "${MATRIX_HELPERS}" '--data-binary @-' \
	'Matrix bootstrap documents are not piped to curl over stdin'
if awk 'NR <= 110' "${MATRIX_HELPERS}" |
	rg --quiet --regexp 'openssl sha1 -hmac|--arg password|--data "\$\{document\}"'; then
	fail 'Matrix bootstrap credentials still reach a child process through argv'
fi
assert_contains "${SIGNING_HELPERS}" \
	'federation_kubectl A --namespace matrix-c exec --stdin' \
	'C signing is not pinned to control plane A'
if rg --line-number --regexp '(^|[^_[:alnum:]])kubectl[[:space:]]' \
	"${A2A_HELPERS}" "${MATRIX_HELPERS}" "${SIGNING_HELPERS}" >/dev/null; then
	fail 'a federation proof helper bypasses federation_kubectl routing'
fi
if rg --quiet --fixed-strings 'federation_kubectl B' \
	"${A2A_HELPERS}" "${MATRIX_HELPERS}" "${SIGNING_HELPERS}"; then
	fail 'a federation workload, signing, or log lookup is routed directly to control plane B'
fi
if rg --quiet --fixed-strings 'bootstrap_secret_value' "${SEED}" "${A2A_HELPERS}"; then
	fail 'split seed still uses the ambient-cluster bootstrap secret helper'
fi
for claim in \
	'A2A driver:    host process; no B-workload-origin A2A claim is made' \
	'Host trust:    distinct A/B public roots verified in the explicit two-root bundle' \
	'Inventory:     A owns Matrix A/C + agent plane; B owns Matrix B + Keycloak only' \
	'C -> A deny:   local to control plane A' \
	'Matrix relay:  bidirectional A/B messages are the cross-network relay proof' \
	"C -> B deny:   signed by C on A; rejected at B's distinct ingress"; do
	assert_contains "${SEED}" "${claim}" "split receipt omits honest boundary: ${claim}"
done
if rg --quiet --regexp 'C -> B.*relay|C-to-B.*relay|C -> B.*crossed|cross-control-plane C-to-B' \
	"${SEED}"; then
	fail 'split seed incorrectly claims that the host-delivered C-to-B denial traverses a relay'
fi

echo 'Split federation seed routing contracts passed.'
