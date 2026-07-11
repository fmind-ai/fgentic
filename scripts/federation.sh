#!/usr/bin/env bash
# Provider-free lifecycle for the disposable two-homeserver federation lab. The generic demo
# installer owns the shared mechanics; this wrapper fixes the profile, cluster, and deletion guard.
set -euo pipefail

readonly ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly FEDERATION_CLUSTER="fgentic-fed"

usage() {
	cat <<'EOF'
usage: scripts/federation.sh up|down

Creates the owned fgentic-fed k3d cluster with two provider-free Synapse homeservers:
  org-a.fgentic.localhost
  org-b.fgentic.localhost

`up` reconciles the lab, proves a bidirectional federated room exchange, and leaves the cluster
running for inspection. `down` deletes only that ownership-labelled cluster and its local images.

Environment:
  FGENTIC_FED_CLUSTER  must be fgentic-fed when set
  FGENTIC_FED_TIMEOUT  reconciliation timeout (default: 20m)
  FGENTIC_DEMO_CACHE_DIR
                       optional persistent BuildKit cache directory for the source image
EOF
}

die() {
	echo "error: $*" >&2
	exit 1
}

if (($# != 1)); then
	usage >&2
	exit 2
fi

cluster_name="${FGENTIC_FED_CLUSTER:-${FEDERATION_CLUSTER}}"
[ "${cluster_name}" = "${FEDERATION_CLUSTER}" ] ||
	die "FGENTIC_FED_CLUSTER must be ${FEDERATION_CLUSTER}"

case "$1" in
up | down)
	export FGENTIC_DEMO_PROFILE=federation
	export FGENTIC_DEMO_CLUSTER="${cluster_name}"
	export FGENTIC_DEMO_TIMEOUT="${FGENTIC_FED_TIMEOUT:-20m}"
	exec "${ROOT_DIR}/scripts/demo.sh" "$1"
	;;
-h | --help)
	usage
	;;
*)
	usage >&2
	exit 2
	;;
esac
