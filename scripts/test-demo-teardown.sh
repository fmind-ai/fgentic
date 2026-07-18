#!/usr/bin/env bash
# Deterministic command fakes for crash-resumable demo/federation teardown. No Docker daemon,
# k3d cluster, kubeconfig, credential, or network endpoint is accessed by this test.
# shellcheck disable=SC2030,SC2031 # fixture cases intentionally isolate injected globals in subshells
# shellcheck disable=SC2312 # substitution output is consumed by explicit fixture assertions
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly DEMO="${ROOT_DIR}/scripts/demo.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-teardown-check.XXXXXX")"
readonly WORK_DIR
readonly FAKE_BIN="${WORK_DIR}/bin"
readonly CLUSTER_NAME="fgentic-demo-teardown"
mkdir -p "${FAKE_BIN}"
cleanup() {
	if [ "${FGENTIC_TEST_KEEP_WORK_DIR:-no}" = yes ]; then
		echo "Preserving teardown fixture state: ${WORK_DIR}" >&2
	else
		rm -rf "${WORK_DIR}"
	fi
}
trap cleanup EXIT INT TERM

fail() {
	echo "error: $*" >&2
	exit 1
}

cat >"${FAKE_BIN}/k3d" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${FAKE_DOCKER_STATE:?}"
case "${1:-} ${2:-}" in
"cluster list")
	if [ -f "${state}/cluster" ]; then
		printf '[{"name":"%s"}]\n' "${FAKE_CLUSTER_NAME:?}"
	else
		printf '[]\n'
	fi
	;;
"cluster delete")
	receipt="${FGENTIC_DEMO_STATE_DIR:?}/cluster-teardown/${FAKE_CLUSTER_NAME:?}.json"
	[ -f "${receipt}" ] || {
		echo 'error: cluster deletion started before the teardown receipt was committed' >&2
		exit 1
	}
	jq --exit-status '.schema == "fgentic.cluster-teardown.v1"' "${receipt}" >/dev/null || {
		echo 'error: cluster deletion started with an invalid teardown receipt' >&2
		exit 1
	}
	printf 'cluster-delete\n' >>"${state}/commands"
	rm -f "${state}/cluster"
	if [ "${FAKE_TEARDOWN_SCENARIO:-transient}" = clean ]; then
		rm -f "${state}/server" "${state}/loadbalancer" "${state}/network" \
			"${state}/volume-images" "${state}/volume-anonymous" "${state}/image"
	fi
	;;
*) exit 2 ;;
esac
EOF

cat >"${FAKE_BIN}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${FAKE_DOCKER_STATE:?}"
cluster="${FAKE_CLUSTER_NAME:?}"

container_document() {
	local id="$1"
	case "${id}" in
	"$(cat "${state}/server" 2>/dev/null || true)" | "k3d-${cluster}-server-0")
		[ -f "${state}/server" ] || return 1
		jq --null-input --arg id "$(cat "${state}/server")" --arg cluster "${cluster}" \
			--arg owner "$(cat "${state}/owner")" '{
          Id: $id,
          Name: ("/k3d-" + $cluster + "-server-0"),
          Config: {Labels: {
            "k3d.cluster": $cluster,
            "dev.fgentic.demo": $owner
          }},
          Mounts: [
            {Type: "volume", Name: ("k3d-" + $cluster + "-images")},
            {Type: "volume", Name: "fixture-anonymous-volume"}
          ]
        }'
		;;
	"$(cat "${state}/loadbalancer" 2>/dev/null || true)" | "k3d-${cluster}-serverlb")
		[ -f "${state}/loadbalancer" ] || return 1
		jq --null-input --arg id "$(cat "${state}/loadbalancer")" --arg cluster "${cluster}" '{
          Id: $id,
          Name: ("/k3d-" + $cluster + "-serverlb"),
          Config: {Labels: {"k3d.cluster": $cluster}},
          Mounts: []
        }'
		;;
	*) return 1 ;;
	esac
}

inspect_containers() {
	local document id
	local documents=()
	shift 2
	for id in "$@"; do
		document="$(container_document "${id}")" || exit 1
		documents[${#documents[@]}]="${document}"
	done
	printf '%s\n' "${documents[@]}" | jq --slurp '.'
}

volume_document() {
	local name="$1"
	case "${name}" in
	"k3d-${cluster}-images")
		[ -f "${state}/volume-images" ] || return 1
		jq --null-input --arg cluster "${cluster}" \
			--arg created "$(cat "${state}/volume-images")" '{
          Name: ("k3d-" + $cluster + "-images"),
          CreatedAt: $created,
          Mountpoint: "/fixture/images",
          Labels: {app: "k3d", "k3d.cluster": $cluster}
        }'
		;;
	fixture-anonymous-volume)
		[ -f "${state}/volume-anonymous" ] || return 1
		jq --null-input --arg created "$(cat "${state}/volume-anonymous")" '{
          Name: "fixture-anonymous-volume",
          CreatedAt: $created,
          Mountpoint: "/fixture/anonymous",
          Labels: {"com.docker.volume.anonymous": ""}
        }'
		;;
	*) return 1 ;;
	esac
}

case "${1:-}" in
info) exit 0 ;;
inspect)
	if [ "${2:-}" = --format ]; then
		[ "${4:-}" = "k3d-${cluster}-server-0" ] || exit 1
		[ -f "${state}/server" ] || exit 1
		printf '%s\n' "$(cat "${state}/owner")"
	else
		inspect_containers container inspect "${@:2}"
	fi
	;;
container)
	case "${2:-}" in
	inspect) inspect_containers "$@" ;;
	*) exit 2 ;;
	esac
	;;
ps)
	if [ -f "${state}/server" ]; then
		cat "${state}/server"
	fi
	if [ -f "${state}/loadbalancer" ]; then
		cat "${state}/loadbalancer"
	fi
	;;
rm)
	shift
	[ "${1:-}" = --force ] && shift
	[ "${1:-}" = --volumes ] && shift
	for id in "$@"; do
		printf 'container-rm:%s\n' "${id}" >>"${state}/commands"
		if [ -f "${state}/server" ] && [ "${id}" = "$(cat "${state}/server")" ]; then
			rm -f "${state}/server" "${state}/volume-anonymous"
		elif [ -f "${state}/loadbalancer" ] && [ "${id}" = "$(cat "${state}/loadbalancer")" ]; then
			rm -f "${state}/loadbalancer"
		fi
	done
	;;
network)
	case "${2:-}" in
	inspect)
		[ -f "${state}/network" ] || exit 1
		case "${3:-}" in
		"$(cat "${state}/network")" | "k3d-${cluster}") ;;
		*) exit 1 ;;
		esac
		jq --null-input --arg id "$(cat "${state}/network")" --arg cluster "${cluster}" \
			'[{Id: $id, Name: ("k3d-" + $cluster), Labels: {app: "k3d", "k3d.cluster": $cluster}}]'
		;;
	rm)
		printf 'network-rm:%s\n' "${3:-}" >>"${state}/commands"
		[ -f "${state}/network" ] && [ "${3:-}" = "$(cat "${state}/network")" ] &&
			rm -f "${state}/network"
		;;
	*) exit 2 ;;
	esac
	;;
volume)
	case "${2:-}" in
	inspect)
		shift 2
		documents=()
		for name in "$@"; do
			document="$(volume_document "${name}")" || exit 1
			documents[${#documents[@]}]="${document}"
		done
		printf '%s\n' "${documents[@]}" | jq --slurp '.'
		;;
	rm)
		printf 'volume-rm:%s\n' "${3:-}" >>"${state}/commands"
		case "${3:-}" in
		"k3d-${cluster}-images") rm -f "${state}/volume-images" ;;
		fixture-anonymous-volume) rm -f "${state}/volume-anonymous" ;;
		esac
		;;
	*) exit 2 ;;
	esac
	;;
images)
	if [ -f "${state}/image" ]; then
		cat "${state}/image"
	fi
	;;
image)
	case "${2:-}" in
	inspect)
		[ -f "${state}/image" ] || exit 1
		case "${3:-}" in
		"$(cat "${state}/image")" | fixture/image:owned) ;;
		*) exit 1 ;;
		esac
		jq --null-input --arg id "$(cat "${state}/image")" --arg cluster "${cluster}" \
			--arg owner "$(cat "${state}/image-owner")" '[{
          Id: $id,
          RepoTags: ["fixture/image:owned"],
          Config: {Labels: {"dev.fgentic.demo.cluster": $owner}}
        }]'
		;;
	rm)
		shift 2
		[ "${1:-}" = --force ] && shift
		printf 'image-rm:%s\n' "${1:-}" >>"${state}/commands"
		[ -f "${state}/image" ] && [ "${1:-}" = "$(cat "${state}/image")" ] &&
			rm -f "${state}/image"
		;;
	*) exit 2 ;;
	esac
	;;
*) exit 2 ;;
esac
EOF

cat >"${FAKE_BIN}/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "${FAKE_BIN}/docker" "${FAKE_BIN}/k3d" "${FAKE_BIN}/sleep"

initialize_state() {
	local state="$1" owner="${2:-true}"
	mkdir -p "${state}"
	touch "${state}/cluster"
	printf '%s\n' "${owner}" >"${state}/owner"
	printf '%s\n' container-server-id >"${state}/server"
	printf '%s\n' container-loadbalancer-id >"${state}/loadbalancer"
	printf '%s\n' network-id >"${state}/network"
	printf '%s\n' 2026-07-15T00:00:00Z >"${state}/volume-images"
	printf '%s\n' 2026-07-15T00:00:01Z >"${state}/volume-anonymous"
	printf '%s\n' sha256:owned-image-id >"${state}/image"
	printf '%s\n' "${CLUSTER_NAME}" >"${state}/image-owner"
	printf '%s\n' caller-context >"${state}/caller-kubeconfig"
	: >"${state}/commands"
}

receipt_path() {
	local state="$1"
	printf '%s/lifecycle/cluster-teardown/%s.json\n' "${state}" "${CLUSTER_NAME}"
}

create_receipt() {
	local state="$1"
	(
		export PATH="${FAKE_BIN}:${PATH}"
		export FAKE_DOCKER_STATE="${state}"
		export FAKE_CLUSTER_NAME="${CLUSTER_NAME}"
		export FGENTIC_DEMO_STATE_DIR="${state}/lifecycle"
		# shellcheck source=scripts/lib.sh
		source "${ROOT_DIR}/scripts/lib.sh"
		# shellcheck source=scripts/lib/demo-cluster.sh
		source "${ROOT_DIR}/scripts/lib/demo-cluster.sh"
		PROFILE=demo
		OWNER_LABEL=true
		write_teardown_receipt
	)
}

run_demo() {
	local action="$1" state="$2" scenario="${3:-transient}"
	PATH="${FAKE_BIN}:${PATH}" \
		FAKE_DOCKER_STATE="${state}" FAKE_CLUSTER_NAME="${CLUSTER_NAME}" \
		FAKE_TEARDOWN_SCENARIO="${scenario}" FGENTIC_DEMO_STATE_DIR="${state}/lifecycle" \
		FGENTIC_DEMO_CLUSTER="${CLUSTER_NAME}" KUBECONFIG="${state}/caller-kubeconfig" \
		"${DEMO}" "${action}"
}

assert_clean() {
	local resource state="$1"
	for resource in cluster server loadbalancer network volume-images volume-anonymous image; do
		[ ! -e "${state}/${resource}" ] || fail "teardown retained ${resource}"
	done
	[ ! -e "$(receipt_path "${state}")" ] || fail 'teardown retained its completed receipt'
	[ "$(cat "${state}/caller-kubeconfig")" = caller-context ] \
		|| fail 'teardown changed the caller kubeconfig'
}

clean_state="${WORK_DIR}/clean"
initialize_state "${clean_state}"
run_demo down "${clean_state}" clean >"${clean_state}/output" 2>&1
assert_clean "${clean_state}"
rg --fixed-strings 'were preserved' "${clean_state}/output" >/dev/null
run_demo down "${clean_state}" transient >"${clean_state}/idempotent" 2>&1
rg --fixed-strings 'does not exist' "${clean_state}/idempotent" >/dev/null

for boundary in receipt metadata server partial; do
	state="${WORK_DIR}/interrupt-${boundary}"
	initialize_state "${state}"
	create_receipt "${state}"
	receipt="$(receipt_path "${state}")"
	jq --exit-status '
    .schema == "fgentic.cluster-teardown.v1" and
    .generation == "container-server-id" and
    (.containers | length == 2) and
    (.network.id == "network-id") and
    ([.volumes[].kind] | sort == ["anonymous", "images"]) and
    (.images[0].id == "sha256:owned-image-id") and
    (paths(scalars) as $path | getpath($path) | type == "string")
  ' "${receipt}" >/dev/null || fail "${boundary} receipt schema drifted"
	if rg --ignore-case 'password|credential|secret|token|matrix|a2a|content|log' "${receipt}" >/dev/null; then
		fail "${boundary} receipt contains forbidden content classes"
	fi
	case "${boundary}" in
		receipt) ;;
		metadata) rm -f "${state}/cluster" ;;
		server) rm -f "${state}/cluster" "${state}/server" ;;
		partial)
			rm -f "${state}/cluster" "${state}/server" "${state}/loadbalancer" \
				"${state}/network" "${state}/volume-images" "${state}/image"
			;;
		*) fail "unsupported teardown interruption boundary: ${boundary}" ;;
	esac

	if [ "${boundary}" = receipt ]; then
		run_demo status "${state}" >"${state}/status" 2>&1
		rg --fixed-strings 'state=recovery-pending' "${state}/status" >/dev/null
		[ ! -s "${state}/commands" ] || fail 'status mutated pending teardown state'
		if run_demo up "${state}" >"${state}/up" 2>&1; then
			fail 'up ignored a pending teardown receipt'
		fi
		rg --fixed-strings 'run the matching down command before up' "${state}/up" >/dev/null
		[ ! -s "${state}/commands" ] || fail 'up mutated pending teardown state'
	fi
	run_demo down "${state}" transient >"${state}/recovery" 2>&1
	assert_clean "${state}"
	rg --fixed-strings 'Recovered teardown' "${state}/recovery" >/dev/null
	if rg --fixed-strings 'kubeconfig get' "${state}/commands" >/dev/null; then
		fail "${boundary} recovery touched a Kubernetes context"
	fi
done

for conflict in server network volume image; do
	state="${WORK_DIR}/conflict-${conflict}"
	initialize_state "${state}"
	create_receipt "${state}"
	case "${conflict}" in
		server) printf '%s\n' replacement-server-id >"${state}/server" ;;
		network) printf '%s\n' replacement-network-id >"${state}/network" ;;
		volume) printf '%s\n' 2026-07-16T00:00:00Z >"${state}/volume-images" ;;
		image)
			printf '%s\n' sha256:replacement-image-id >"${state}/image"
			printf '%s\n' foreign >"${state}/image-owner"
			;;
		*) fail "unsupported teardown conflict: ${conflict}" ;;
	esac
	if run_demo down "${state}" transient >"${state}/conflict" 2>&1; then
		fail "${conflict} name reuse was accepted"
	fi
	[ -f "${state}/cluster" ] || fail "${conflict} conflict mutated cluster metadata"
	[ -f "$(receipt_path "${state}")" ] || fail "${conflict} conflict cleared recovery evidence"
	if rg --fixed-strings 'cluster-delete' "${state}/commands" >/dev/null; then
		fail "${conflict} conflict reached destructive cleanup"
	fi
	rg --regexp 'reused|changed' "${state}/conflict" >/dev/null \
		|| fail "${conflict} conflict lacked actionable diagnostics"
done

foreign_state="${WORK_DIR}/foreign"
initialize_state "${foreign_state}" foreign
if run_demo down "${foreign_state}" transient >"${foreign_state}/output" 2>&1; then
	fail 'initial teardown accepted a foreign server owner'
fi
[ -f "${foreign_state}/cluster" ] && [ ! -e "$(receipt_path "${foreign_state}")" ] \
	|| fail 'foreign-owner control was mutated'

orphan_state="${WORK_DIR}/orphan"
initialize_state "${orphan_state}"
rm -f "${orphan_state}/cluster" "${orphan_state}/server"
if run_demo down "${orphan_state}" transient >"${orphan_state}/output" 2>&1; then
	fail 'receipt-free orphan was adopted'
fi
rg --fixed-strings 'teardown receipt and owner-labelled server evidence are unavailable' \
	"${orphan_state}/output" >/dev/null

malformed_state="${WORK_DIR}/malformed"
initialize_state "${malformed_state}"
mkdir -p "$(dirname "$(receipt_path "${malformed_state}")")"
printf '{}\n' >"$(receipt_path "${malformed_state}")"
if run_demo down "${malformed_state}" transient >"${malformed_state}/output" 2>&1; then
	fail 'malformed receipt was accepted'
fi
[ -f "${malformed_state}/cluster" ] || fail 'malformed receipt mutated cluster metadata'
rg --fixed-strings 'malformed or stale teardown receipt' "${malformed_state}/output" >/dev/null
rg --fixed-strings 'inspect only: jq .' "${malformed_state}/output" >/dev/null

if rg --regexp 'stat -c|readlink -f|flock' "${ROOT_DIR}/scripts/lib/demo-cluster.sh" >/dev/null; then
	fail 'teardown receipt uses a Linux-only filesystem primitive'
fi

echo 'Crash-resumable demo/federation teardown fixtures passed.'
