#!/usr/bin/env bash
# Focused offline contracts for split-federation failure cleanup. No Docker daemon,
# Kubernetes API, credential, or network endpoint is accessed by this test.
set -euo pipefail

TEST_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_ROOT
readonly SPLIT="${TEST_ROOT}/scripts/federation-split.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-split-lifecycle-check.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

fail() {
	echo "error: $*" >&2
	exit 1
}

# main is guarded, so sourcing exposes only the lifecycle functions and constants.
export FGENTIC_DEMO_STATE_DIR="${WORK_DIR}/state"
# shellcheck source=scripts/federation-split.sh
source "${SPLIT}"

run_failure_fixture() {
	local cleanup_dir="$1"
	SPLIT_UP_WORK_DIR="${cleanup_dir}"
	trap 'cleanup_split_up "$?"' EXIT
	false
}

failure_dir="${WORK_DIR}/command-failure"
mkdir "${failure_dir}"
set +e
failure_output="$(
	exec 2>&1
	set -Eeuo pipefail
	run_failure_fixture "${failure_dir}"
)"
failure_status=$?
set -e
[ "${failure_status}" -eq 1 ] || fail "command failure status was not preserved"
[ ! -e "${failure_dir}" ] || fail "command failure left its scoped work directory"
[ "${failure_output}" = "Split federation did not complete; run fed:split:status, then fed:split:down for exact recovery." ] ||
	fail "command failure recovery guidance changed"

signal_dir="${WORK_DIR}/signal"
mkdir "${signal_dir}"
set +e
signal_output="$(
	exec 2>&1
	set -Eeuo pipefail
	# The EXIT trap consumes this global through cleanup_split_up.
	# shellcheck disable=SC2034
	SPLIT_UP_WORK_DIR="${signal_dir}"
	trap 'cleanup_split_up "$?"' EXIT
	trap 'exit 143' TERM
	kill -TERM "${BASHPID}"
)"
signal_status=$?
set -e
[ "${signal_status}" -eq 143 ] || fail "termination status was not preserved"
[ ! -e "${signal_dir}" ] || fail "termination left its scoped work directory"
[ "${signal_output}" = "Split federation did not complete; run fed:split:status, then fed:split:down for exact recovery." ] ||
	fail "termination recovery guidance changed"

up_events="${WORK_DIR}/up-events"
set +e
(
	set -e
	require_canonical_absent() { printf 'canonical\n' >>"${up_events}"; }
	child_preflight_state() {
		printf 'child:%s\n' "$2" >>"${up_events}"
		printf 'absent\n'
	}
	preflight_split_up() {
		printf 'parent-preflight\n' >>"${up_events}"
		return 1
	}
	prepare_public_roots() { printf 'MUTATION:ca\n' >>"${up_events}"; }
	run_child() { printf 'MUTATION:child\n' >>"${up_events}"; }
	split_up
) >"${WORK_DIR}/up-preflight-output" 2>&1
up_status=$?
set -e
[ "${up_status}" -ne 0 ] || fail "split up accepted a failed parent preflight"
if rg --fixed-strings 'MUTATION:' "${up_events}" >/dev/null; then
	fail "split up mutated lifecycle state before its parent preflight passed"
fi
[ "$(tr '\n' ',' <"${up_events}")" = \
	'canonical,child:fgentic-fed-a,child:fgentic-fed-b,parent-preflight,' ] ||
	fail "split up preflight ordering changed"

down_events="${WORK_DIR}/down-events"
mkdir -p "${SPLIT_STATE_DIR}"
rm -f "${PARENT_TEARDOWN_RECEIPT}"
(
	assert_parent() {
		[ -f "${PARENT_TEARDOWN_RECEIPT}" ] ||
			fail "destructive split-down phase ran before the parent receipt"
	}
	prepare_parent_teardown_receipt() {
		printf 'parent\n' >>"${down_events}"
		printf '{}\n' >"${PARENT_TEARDOWN_RECEIPT}"
	}
	validate_parent_teardown_resources() {
		assert_parent
		printf 'validate\n' >>"${down_events}"
	}
	remove_relay_receipt_resources() {
		assert_parent
		printf 'relay\n' >>"${down_events}"
	}
	remove_parent_child_resources() {
		assert_parent
		printf 'child:%s\n' "$1" >>"${down_events}"
	}
	remove_parent_ca_state() {
		assert_parent
		printf 'ca\n' >>"${down_events}"
	}
	complete_parent_teardown_receipt() {
		assert_parent
		printf 'complete\n' >>"${down_events}"
		rm -f "${PARENT_TEARDOWN_RECEIPT}"
	}
	split_down
) >"${WORK_DIR}/down-order-output"
parent_line="$(rg -n '^parent$' "${down_events}" | cut -d: -f1)"
relay_line="$(rg -n '^relay$' "${down_events}" | cut -d: -f1)"
child_b_line="$(rg -n '^child:b$' "${down_events}" | cut -d: -f1)"
child_a_line="$(rg -n '^child:a$' "${down_events}" | cut -d: -f1)"
ca_line="$(rg -n '^ca$' "${down_events}" | cut -d: -f1)"
complete_line="$(rg -n '^complete$' "${down_events}" | cut -d: -f1)"
((parent_line < relay_line && relay_line < child_b_line &&
	child_b_line < child_a_line && child_a_line < ca_line &&
	ca_line < complete_line)) ||
	fail "split down no longer commits parent -> relays -> B -> A -> CA -> complete"

: >"${down_events}"
rm -f "${PARENT_TEARDOWN_RECEIPT}"
set +e
(
	set -e
	prepare_parent_teardown_receipt() {
		printf 'preflight-rejected\n' >>"${down_events}"
		return 1
	}
	remove_relay_receipt_resources() { printf 'MUTATION:relay\n' >>"${down_events}"; }
	remove_parent_child_resources() { printf 'MUTATION:child\n' >>"${down_events}"; }
	remove_parent_ca_state() { printf 'MUTATION:ca\n' >>"${down_events}"; }
	complete_parent_teardown_receipt() { printf 'MUTATION:complete\n' >>"${down_events}"; }
	split_down
) >"${WORK_DIR}/down-preflight-output" 2>&1
down_status=$?
set -e
[ "${down_status}" -ne 0 ] || fail "split down accepted a failed parent preflight"
if rg --fixed-strings 'MUTATION:' "${down_events}" >/dev/null; then
	fail "split down mutated runtime state after a failed parent preflight"
fi

printf 'preserve-until-live-preflight-passes\n' \
	>"${SPLIT_STATE_DIR}/.relays.preserved"
set +e
(
	set -e
	validate_stable_ca_state() { printf 'absent\n'; }
	child_down_preflight_state() { printf 'absent\n'; }
	preflight_relay_teardown_state() { return 1; }
	prepare_parent_teardown_receipt
) >"${WORK_DIR}/temporary-preflight-output" 2>&1
temporary_preflight_status=$?
set -e
[ "${temporary_preflight_status}" -ne 0 ] ||
	fail "split parent preflight accepted its rejected relay inventory"
[ -f "${SPLIT_STATE_DIR}/.relays.preserved" ] ||
	fail "split down removed atomic state before the complete live preflight passed"
rm -f "${SPLIT_STATE_DIR}/.relays.preserved"

printf '{}\n' >"${PARENT_TEARDOWN_RECEIPT}"
printf '{"phase":"removing"}\n' >"${RELAY_RECEIPT}"
status_events="${WORK_DIR}/status-events"
(
	validate_parent_teardown_receipt_file() { return 0; }
	validate_parent_teardown_resources() { return 0; }
	validate_relay_receipt_file() { return 0; }
	preflight_relay_teardown_state() {
		printf 'INVALID:pre-parent-relay-preflight\n' >>"${status_events}"
		return 1
	}
	relay_containers_present() { return 1; }
	run_child() {
		printf 'Federation cluster %s: state=absent retained_bytes=0\n' "$2"
	}
	split_status
) >"${WORK_DIR}/parent-partial-status-output"
[ ! -e "${status_events}" ] ||
	fail "parent-receipted partial status reran the pre-parent relay preflight"
rg --fixed-strings 'state=recovery-pending phase=removing' \
	"${WORK_DIR}/parent-partial-status-output" >/dev/null
rm -f "${PARENT_TEARDOWN_RECEIPT}" "${RELAY_RECEIPT}"

cat >"${RELAY_RECEIPT}" <<'EOF'
{"relays":[
  {"direction":"a-to-b","id":""},
  {"direction":"b-to-a","id":""}
]}
EOF
(
	finalize_relay_identity_for_teardown() {
		local direction="$1" id temporary
		case "${direction}" in
		a-to-b) id=relay-a-after-create ;;
		b-to-a) id=relay-b-after-create ;;
		esac
		temporary="${RELAY_RECEIPT}.next"
		jq --arg direction "${direction}" --arg id "${id}" '
      (.relays[] | select(.direction == $direction).id) = $id
    ' "${RELAY_RECEIPT}" >"${temporary}"
		mv "${temporary}" "${RELAY_RECEIPT}"
	}
	owner_relay_ids() {
		printf '%s\n' '["relay-a-after-create","relay-b-after-create"]'
	}
	assert_live_relay_inventory
)
jq -e '
  [.relays[].id] | sort ==
    ["relay-a-after-create", "relay-b-after-create"]
' "${RELAY_RECEIPT}" >/dev/null ||
	fail "interrupted relay create did not normalize its nonce-verified live IDs"
rm -f "${RELAY_RECEIPT}"

hash_a="sha256:$(printf '%064d' 0)"
hash_b="sha256:$(printf '%064d' 1)"
child_a="$(jq -cn --arg hash "${hash_a}" '{
  state: "present", layout: "split-a", cluster: "fgentic-fed-a",
  owner: "federation-split-a", receipt_sha256: $hash, generation: "server-a",
  server: {id: "server-a", name: "k3d-fgentic-fed-a-server-0"},
  network: {id: "network-a", name: "k3d-fgentic-fed-a", cluster_label: "fgentic-fed-a"},
  image_volume: {name: "k3d-fgentic-fed-a-images", created_at: "fixture-a",
    kind: "images", attachments: ["server-a"]}
}')"
child_b="$(jq -cn --arg hash "${hash_a}" '{
  state: "present", layout: "split-b", cluster: "fgentic-fed-b",
  owner: "federation-split-b", receipt_sha256: $hash, generation: "server-b",
  server: {id: "server-b", name: "k3d-fgentic-fed-b-server-0"},
  network: {id: "network-b", name: "k3d-fgentic-fed-b", cluster_label: "fgentic-fed-b"},
  image_volume: {name: "k3d-fgentic-fed-b-images", created_at: "fixture-b",
    kind: "images", attachments: ["server-b"]}
}')"
ca_snapshot="$(jq -cn --arg hash_a "${hash_a}" --arg hash_b "${hash_b}" '{
  state: "present", receipt_sha256: $hash_a, roots: {a: $hash_a, b: $hash_b},
  files: [
    "ca/org-a/ca.crt", "ca/org-a/ca.key", "ca/org-b/ca.crt", "ca/org-b/ca.key",
    "ca/host-bundle.pem", "ca/roots.json"
  ] | map({path: ., sha256: $hash_a})
}')"
relay_source="$(jq -cn --arg image "${RELAY_IMAGE}" --arg hash "${hash_a}" '{
  schema: "fgentic.federation-split-relays.v1", phase: "removing",
  image: $image, image_id: "relay-image",
  networks: {
    a: {id: "network-a", name: "k3d-fgentic-fed-a", cluster: "fgentic-fed-a"},
    b: {id: "network-b", name: "k3d-fgentic-fed-b", cluster: "fgentic-fed-b"}
  },
  relays: [
    {direction: "a-to-b", generation: "00000000000000000000000000000001",
      id: "relay-a", name: "fgentic-fed-a-to-b", local_network: "k3d-fgentic-fed-a",
      remote_network: "k3d-fgentic-fed-b", local_ip: "172.18.0.10",
      target: "k3d-fgentic-fed-b-serverlb", config_sha256: $hash},
    {direction: "b-to-a", generation: "00000000000000000000000000000002",
      id: "relay-b", name: "fgentic-fed-b-to-a", local_network: "k3d-fgentic-fed-b",
      remote_network: "k3d-fgentic-fed-a", local_ip: "172.19.0.10",
      target: "k3d-fgentic-fed-a-serverlb", config_sha256: $hash}
  ]
}')"
relay_snapshot="$(jq -cn --arg hash "${hash_a}" --argjson receipt "${relay_source}" '{
  state: "present", source_phase: "active", receipt_sha256: $hash, receipt: $receipt,
  inventory: [
    {direction: "a-to-b", state: "present", id: "relay-a", name: "fgentic-fed-a-to-b"},
    {direction: "b-to-a", state: "present", id: "relay-b", name: "fgentic-fed-b-to-a"}
  ]
}')"
write_parent_teardown_receipt "${child_a}" "${child_b}" \
	"${ca_snapshot}" "${relay_snapshot}"
validate_parent_teardown_receipt_file ||
	fail "complete parent teardown receipt did not validate"
jq -e '
  .phase == "removing" and
  .children.a.generation == "server-a" and .children.b.generation == "server-b" and
  .children.a.network.id == .relays.receipt.networks.a.id and
  .children.b.image_volume.created_at == "fixture-b" and
  ([.relays.inventory[].state] | all(. == "present")) and
  (.ca.files | length == 6)
' "${PARENT_TEARDOWN_RECEIPT}" >/dev/null ||
	fail "parent teardown receipt omitted a destructive generation identity"
tampered_parent="${WORK_DIR}/tampered-parent.json"
jq '.children.b.network.id = "replacement-network"' \
	"${PARENT_TEARDOWN_RECEIPT}" >"${tampered_parent}"
if validate_parent_teardown_receipt_file "${tampered_parent}"; then
	fail "parent teardown receipt accepted a child/relay network mismatch"
fi
rm -f "${PARENT_TEARDOWN_RECEIPT}"

mkdir -p "${SPLIT_STATE_DIR}/.ca.interrupted/org-a" "${RELAY_CONFIG_DIR}"
printf 'partial-key\n' >"${SPLIT_STATE_DIR}/.ca.interrupted/org-a/ca.key"
printf 'partial-receipt\n' >"${SPLIT_STATE_DIR}/.relays.interrupted"
printf 'partial-config\n' >"${RELAY_CONFIG_DIR}/.relay.interrupted"
validate_temporary_state
cleanup_temporary_state
[ ! -e "${SPLIT_STATE_DIR}/.ca.interrupted" ] &&
	[ ! -e "${SPLIT_STATE_DIR}/.relays.interrupted" ] &&
	[ ! -e "${RELAY_CONFIG_DIR}/.relay.interrupted" ] ||
	fail "split down did not recover exact atomic temporary state"
touch "${WORK_DIR}/atomic-target"
ln -s "${WORK_DIR}/atomic-target" "${SPLIT_STATE_DIR}/.relays.symlink"
if (cleanup_temporary_state) >"${WORK_DIR}/temporary-symlink-output" 2>&1; then
	fail "split atomic recovery accepted a symlink"
fi
[ -f "${WORK_DIR}/atomic-target" ] && [ -L "${SPLIT_STATE_DIR}/.relays.symlink" ] ||
	fail "split atomic recovery followed or removed a symlink target"
rm -f "${SPLIT_STATE_DIR}/.relays.symlink"
rmdir "${RELAY_CONFIG_DIR}" 2>/dev/null || true

rm -rf "${SPLIT_STATE_DIR}"
prepare_public_roots absent absent
validate_stable_ca_state >/dev/null
printf 'unexpected\n' >"${CA_DIR_A}/unreceipted.pem"
if (validate_stable_ca_state) >"${WORK_DIR}/extra-ca-file-output" 2>&1; then
	fail "split CA validation accepted an unreceipted file"
fi
rg --fixed-strings 'unexpected path in split CA directory' \
	"${WORK_DIR}/extra-ca-file-output" >/dev/null
rm -f "${CA_DIR_A}/unreceipted.pem"
ln -s "${WORK_DIR}/atomic-target" "${CA_DIR_B}/unreceipted.pem"
if (validate_stable_ca_state) >"${WORK_DIR}/symlink-ca-file-output" 2>&1; then
	fail "split CA validation accepted an unreceipted symlink"
fi
rg --fixed-strings 'split CA state contains a symlink' \
	"${WORK_DIR}/symlink-ca-file-output" >/dev/null
rm -f "${CA_DIR_B}/unreceipted.pem"
cp "${HOST_CA_BUNDLE}" "${WORK_DIR}/valid-host-bundle.pem"
awk 'NF {print}' "${CA_DIR_A}/ca.crt" >"${HOST_CA_BUNDLE}"
if (validate_stable_ca_state) >"${WORK_DIR}/host-bundle-mismatch-output" 2>&1; then
	fail "split CA validation accepted a host bundle missing the second root"
fi
rg --fixed-strings 'host CA bundle differs' \
	"${WORK_DIR}/host-bundle-mismatch-output" >/dev/null
cp "${WORK_DIR}/valid-host-bundle.pem" "${HOST_CA_BUNDLE}"
rm -f "${CA_RECEIPT}"
if (validate_stable_ca_state) >"${WORK_DIR}/missing-ca-receipt-output" 2>&1; then
	fail "split CA validation accepted a missing identity receipt"
fi
rg --fixed-strings 'partial or unreceipted' \
	"${WORK_DIR}/missing-ca-receipt-output" >/dev/null

echo "Split federation failure cleanup contracts passed."
