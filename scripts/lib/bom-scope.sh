#!/usr/bin/env bash
# Shared scope classification for the adopter Bill of Materials (issue #188).
#
# The BOM enumerates every pinned chart and digest-pinned image that an adopter actually deploys
# through the REFERENCE RELEASE PROFILE: the reconciled `clusters/local` + `clusters/gcp` Flux DAG
# under `infra/` plus the release `deploy/` unit under `apps/matrix-a2a-bridge`, with the tracked
# default `platform-settings` (llm_provider=vertex; admin/canary/alert-delivery/knowledge/embeddings
# profiles disabled; no external mautrix bridge composed).
#
# Both scripts/gen-bom.sh (which pins the BOM) and scripts/check-bom.sh (which fails closed on
# drift) source this file so the in-scope set and the exclusion allowlist have a single source of
# truth. A pin-bearing file that is NEITHER listed in-scope here NOR matched by an exclusion glob
# fails the gate — silent under-coverage is impossible by construction.

# Files whose digest-pinned images are reconciled by the reference profile.
readonly BOM_IMAGE_FILES=(
	"apps/matrix-a2a-bridge/deploy/helmrelease.yaml"
	"infra/agentgateway/mcp-rate-limit.yaml"
	"infra/kagent/helmrelease.yaml"
	"infra/postgres/cluster.yaml"
	"infra/postgres/knowledge-schema-v1.yaml"
	"infra/trivy-operator/helmrelease.yaml"
)

# Files that carry a pinned chart source or a HelmRelease chart version reconciled by the reference
# profile. The `infra/matrix` and `infra/agentgateway` HelmReleases pin their chart through an
# OCIRepository declared in `infra/flux/sources.yaml`; `infra/trivy-operator` pins its chart to an
# immutable Git commit in `infra/trivy-operator/source.yaml`.
readonly BOM_CHART_FILES=(
	"apps/matrix-a2a-bridge/deploy/helmrelease.yaml"
	"infra/agentgateway/helmrelease.yaml"
	"infra/flux/releases.yaml"
	"infra/flux/sources.yaml"
	"infra/kagent/helmrelease.yaml"
	"infra/keycloak/helmrelease.yaml"
	"infra/matrix/helmrelease.yaml"
	"infra/observability/helmrelease.yaml"
	"infra/observability/tracing-helmreleases.yaml"
	"infra/trivy-operator/helmrelease.yaml"
	"infra/trivy-operator/source.yaml"
)

# Exclusion allowlist: `<glob>|<reason>`. Every pin-bearing file that is not in-scope MUST match
# one of these globs, and each carries a one-line reason. Globs are matched with bash `case`, where
# `*` spans `/`. Order is not significant; the classifier stops at the first match.
readonly BOM_EXCLUDED=(
	"*_test.go|Go unit-test fixture image, never deployed"
	"apps/*/scripts/test-*.sh|app test helper script, not a reconciled workload"
	"apps/matrix-a2a-bridge/test/*|bridge integration-test fixtures, not the reference profile"
	"apps/activitypub-agent-gateway/deploy/*|ActivityPub gateway reconciled only on the demo profile (ADR 0014), not local/gcp"
	"apps/matrix-group-sync/deploy/*|opt-in matrix-l reconciler, not referenced by clusters/base/apps.yaml (ADR 0009)"
	"clusters/demo/*|disposable evaluation profile, not the production reference"
	"clusters/*/flux-system/*|Flux bootstrap components (gotk), not workload chart/image pins"
	"infra/federation/*|provider-free federation lab, outside the reconciled reference DAG"
	"infra/admin/*|disabled-by-default Ketesa admin console profile"
	"infra/alert-delivery/*|disabled-by-default alert-delivery profile"
	"infra/canary/*|disabled-by-default synthetic-canary profile"
	"infra/knowledge/*|disabled-by-default knowledge-ingestion profile"
	"infra/models/*|optional self-hosted model runtimes; the reference selects the vertex provider (no in-repo pin)"
	"infra/bridges/*|disabled-by-default external mautrix bridge profiles"
	"infra/moderation/*|disabled-by-default Draupnir policy-list moderation profile"
	"infra/mcp-catalog/*|MCP governance catalog metadata; its kustomization renders no workloads (check:mcp-governance)"
	"infra/agentgateway/mcp-surface.pin.json|MCP surface governance pin, not a reconciled workload (mirrors the in-scope kagent tools digest)"
	"infra/postgres/components/*|opt-in Postgres components (knowledge ingestion), not composed by the reference overlay"
)

# bom_is_in_scope <file> — return 0 when the file is an in-scope pin source.
bom_is_in_scope() {
	local file="$1" candidate
	for candidate in "${BOM_IMAGE_FILES[@]}" "${BOM_CHART_FILES[@]}"; do
		if [ "${file}" = "${candidate}" ]; then
			return 0
		fi
	done
	return 1
}

# bom_exclusion_reason <file> — print the exclusion reason and return 0 when a glob matches,
# otherwise return 1 (the file is unclassified and must fail the gate).
bom_exclusion_reason() {
	local file="$1" entry glob reason
	for entry in "${BOM_EXCLUDED[@]}"; do
		glob="${entry%%|*}"
		reason="${entry#*|}"
		# The allowlist entries are deliberately glob patterns (e.g. `infra/models/*`); the case
		# pattern must expand ${glob} as a glob, not match it literally.
		# shellcheck disable=SC2254
		case "${file}" in
			${glob})
				printf '%s\n' "${reason}"
				return 0
				;;
			*) ;;
		esac
	done
	return 1
}
