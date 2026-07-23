#!/usr/bin/env bash
# shellcheck disable=SC2312 # substitutions feed fail-closed assertions or mandatory fixture execution
# Offline negative-posture proof for the break-glass recovery credential (issue #467, Task 5). It
# exercises the real age/SOPS generator in a disposable fixture and never touches a cluster,
# provider, or production recipient. The invariants under test are the fail-closed default posture:
#   1. a default bootstrap (SECRET_SET=all) emits no break-glass credential;
#   2. the internal `rotatable` sweep never emits it either;
#   3. only an explicit SECRET_SET=break-glass emits a sealed (never plaintext) recovery credential;
#   4. it is bootstrap-once — neither a repeat run nor --force rotates it implicitly;
#   5. no committed cluster secret set ships a break-glass Secret.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
GENERATOR="${SCRIPT_DIR}/gen-secrets.sh"
BREAK_GLASS_FILE="break-glass-recovery.sops.yaml"

for command in age-keygen grep sops yq; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		echo "error: required test command not found: ${command}" >&2
		exit 1
	fi
done

WORK_DIR="$(mktemp -d)"
FIXTURE_ROOT="${WORK_DIR}/fixture"
SECRETS_DIR="${FIXTURE_ROOT}/clusters/local/secrets"
trap 'rm -rf "${WORK_DIR}"' EXIT
mkdir -p "${SECRETS_DIR}"

fail() {
	echo "error: $1" >&2
	exit 1
}

assert_equal() {
	[ "$1" = "$2" ] || fail "$3"
}

secret_value() { # secret_value <namespace> <name> <yq suffix>
	sops --decrypt "${SECRETS_DIR}/${BREAK_GLASS_FILE}" \
		| yq eval-all -er "select(.metadata.namespace == \"$1\" and .metadata.name == \"$2\") | $3" -
}

inventory_lists_break_glass() { # inventory_lists_break_glass <kustomization file>
	[ -f "$1" ] && yq -er '.resources[]' "$1" 2>/dev/null | grep -qx "${BREAK_GLASS_FILE}"
}

age-keygen -o "${WORK_DIR}/age.key" >/dev/null 2>&1
RECIPIENT="$(age-keygen -y "${WORK_DIR}/age.key")"
printf 'creation_rules:\n  - path_regex: \\.sops\\.ya?ml$\n    encrypted_regex: ^(data|stringData)$\n    age: %s\n' \
	"${RECIPIENT}" >"${FIXTURE_ROOT}/.sops.yaml"
printf '%s\n' \
	'apiVersion: v1' \
	'kind: ConfigMap' \
	'metadata:' \
	'  name: platform-settings' \
	'data:' \
	'  llm_provider: vertex' \
	>"${FIXTURE_ROOT}/clusters/local/platform-settings.yaml"

export SOPS_AGE_KEY_FILE="${WORK_DIR}/age.key"
export FGENTIC_DATA_ROOT="${FIXTURE_ROOT}"

# 1. The default posture (SECRET_SET=all) emits nothing break-glass.
"${GENERATOR}" fixture.localhost local >/dev/null
[ ! -e "${SECRETS_DIR}/${BREAK_GLASS_FILE}" ] || fail "default bootstrap emitted a break-glass credential"
if inventory_lists_break_glass "${SECRETS_DIR}/kustomization.yaml"; then
	fail "default kustomization references a break-glass Secret"
fi

# 2. The internal rotatable sweep must also never emit it (excluded like a bootstrap credential).
FGENTIC_SECRET_SET=rotatable "${GENERATOR}" fixture.localhost local --force >/dev/null
[ ! -e "${SECRETS_DIR}/${BREAK_GLASS_FILE}" ] || fail "rotatable sweep emitted a break-glass credential"

# 3. Only the explicit set emits the sealed recovery credential; the password stays encrypted at rest.
FGENTIC_SECRET_SET=break-glass "${GENERATOR}" fixture.localhost local >/dev/null
[ -e "${SECRETS_DIR}/${BREAK_GLASS_FILE}" ] || fail "explicit break-glass set did not emit the credential"
# It must stay SOPS ciphertext in git ONLY -- never globbed into the Flux-reconciled inventory, or
# Flux would decrypt it into an always-standing, unmounted recovery Secret in etcd (#467).
if inventory_lists_break_glass "${SECRETS_DIR}/kustomization.yaml"; then
	fail "break-glass Secret was reconciled into the live inventory (must stay git/SOPS-only)"
fi
assert_equal "break-glass" "$(secret_value matrix break-glass-recovery '.stringData.username')" \
	"break-glass recovery username drifted"
PASSWORD="$(secret_value matrix break-glass-recovery '.stringData.password')"
[ -n "${PASSWORD}" ] || fail "break-glass recovery password is empty"
if grep -qF "${PASSWORD}" "${SECRETS_DIR}/${BREAK_GLASS_FILE}"; then
	fail "break-glass password is stored in cleartext on disk"
fi

# 4. Bootstrap-once: neither a repeat run nor --force rotates the provisioned credential.
FGENTIC_SECRET_SET=break-glass "${GENERATOR}" fixture.localhost local >/dev/null
assert_equal "${PASSWORD}" "$(secret_value matrix break-glass-recovery '.stringData.password')" \
	"break-glass credential rotated implicitly on repeat generation"
FGENTIC_SECRET_SET=break-glass "${GENERATOR}" fixture.localhost local --force >/dev/null
assert_equal "${PASSWORD}" "$(secret_value matrix break-glass-recovery '.stringData.password')" \
	"--force rotated the bootstrap-once break-glass credential"

# 5. No committed cluster secret set ships a break-glass Secret (fail-closed default posture in git).
for dir in "${REPO_ROOT}"/clusters/*/secrets; do
	[ -d "${dir}" ] || continue
	[ ! -e "${dir}/${BREAK_GLASS_FILE}" ] \
		|| fail "committed secret set ships a break-glass credential: ${dir}"
	if inventory_lists_break_glass "${dir}/kustomization.yaml"; then
		fail "committed kustomization references a break-glass Secret: ${dir}"
	fi
done

echo "Break-glass negative-posture proof passed: absent by default and under rotatable, sealed only on explicit request, bootstrap-once, and unshipped in every committed cluster."
