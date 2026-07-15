#!/usr/bin/env bash
# Definition-only disposable k3d config renderer sourced by scripts/demo.sh.

render_k3d_config() {
	local output="$1"
	local eviction_hard="memory.available<100Mi,nodefs.available<1Gi,imagefs.available<1Gi,nodefs.inodesFree<5%,imagefs.inodesFree<5%"
	local audit_policy="k3d-audit-policy.yaml"

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
		if [ "${FEDERATION_CONSTRAINED:-no}" = yes ]; then
			# k3s otherwise grows a host-sized Go heap on the single disposable server. These are
			# creation-time process settings, so lifecycle ownership records the capacity mode.
			yq --inplace '
          .env += [
            {"envVar": "GOGC=50", "nodeFilters": ["server:*"]},
            {"envVar": "GOMEMLIMIT=1GiB", "nodeFilters": ["server:*"]}
          ]
        ' "${output}"
		fi
	fi

	# k3d resolves a relative `files.source` beside the rendered config, not beside the process's
	# working directory. Keep disposable demo/federation configs self-contained without baking a
	# repository-specific absolute path into them.
	cp "${ROOT_DIR}/infra/${audit_policy}" "$(dirname -- "${output}")/${audit_policy}"
}
