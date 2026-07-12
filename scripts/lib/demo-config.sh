#!/usr/bin/env bash
# Definition-only disposable k3d config renderer sourced by scripts/demo.sh.

render_k3d_config() {
	local output="$1"
	local eviction_hard="memory.available<100Mi,nodefs.available<1Gi,imagefs.available<1Gi,nodefs.inodesFree<5%,imagefs.inodesFree<5%"

	# Disposable evaluation clusters pull several large pinned images onto one node. Keep Kubelet
	# from evicting healthy workloads at its percentage-based disk default while preserving an
	# explicit 1 GiB floor; the cluster and its image volume are deleted by demo:down.
	CLUSTER_NAME="${CLUSTER_NAME}" EVICTION_HARD="${eviction_hard}" yq '
      .metadata.name = strenv(CLUSTER_NAME) |
      .options.k3s.extraArgs += [{
        "arg": ("--kubelet-arg=eviction-hard=" + strenv(EVICTION_HARD)),
        "nodeFilters": ["server:*"]
      }]
    ' "${ROOT_DIR}/infra/k3d-config.yaml" >"${output}"

	if [ "${PROFILE}" = "federation" ]; then
		FED_LOOPBACK="${FEDERATION_LOOPBACK}" yq --inplace '
        .ports[0].port = (strenv(FED_LOOPBACK) + ":80:80") |
        .ports[1].port = (strenv(FED_LOOPBACK) + ":443:443")
      ' "${output}"
	fi
}
