#!/usr/bin/env bash
# Fail when a Gateway API route or agentgateway tracing policy targets another namespace without
# an exact ReferenceGrant. ParentRefs are intentionally excluded: cross-namespace Route attachment
# is authorized by the Gateway listener's allowedRoutes, while backendRefs use ReferenceGrant.
set -euo pipefail

if [ "$#" -gt 0 ]; then
	manifests=("$@")
else
	# Rendered charts are validated separately; raw Go-template files are not YAML documents.
	manifest_list="$(rg --files infra clusters -g '*.yaml' -g '*.yml' -g '!**/chart/templates/**' | sort)"
	mapfile -t manifests <<<"${manifest_list}"
fi

if [ "${#manifests[@]}" -eq 0 ]; then
	echo "error: no manifests to audit" >&2
	exit 2
fi

uncovered="$({ yq eval-all -o=json '.' "${manifests[@]}" || exit; } | jq -rs '
  map(select(type == "object")) as $docs
  | (
      [
        $docs[] as $route
        | select(["GRPCRoute", "HTTPRoute", "TCPRoute", "TLSRoute", "UDPRoute"] | index($route.kind))
        | $route.spec.rules[]?.backendRefs[]? as $backend
        | ($route.metadata.namespace // "default") as $source_namespace
        | ($backend.namespace // $source_namespace) as $target_namespace
        | select($target_namespace != $source_namespace)
        | {
            routeGroup: ($route.apiVersion | split("/") | if length == 2 then .[0] else "" end),
            routeKind: $route.kind,
            routeName: $route.metadata.name,
            sourceNamespace: $source_namespace,
            backendGroup: ($backend.group // ""),
            backendKind: ($backend.kind // "Service"),
            backendName: $backend.name,
            targetNamespace: $target_namespace
          }
      ]
      +
      [
        $docs[] as $policy
        | select($policy.apiVersion == "agentgateway.dev/v1alpha1" and $policy.kind == "AgentgatewayPolicy")
        | $policy.spec.frontend.tracing.backendRef? as $backend
        | select($backend != null)
        | ($policy.metadata.namespace // "default") as $source_namespace
        | ($backend.namespace // $source_namespace) as $target_namespace
        | select($target_namespace != $source_namespace)
        | {
            routeGroup: "agentgateway.dev",
            routeKind: "AgentgatewayPolicy",
            routeName: $policy.metadata.name,
            sourceNamespace: $source_namespace,
            backendGroup: ($backend.group // ""),
            backendKind: ($backend.kind // "Service"),
            backendName: $backend.name,
            targetNamespace: $target_namespace
          }
      ]
    )
  | .[]
  | . as $ref
  | select(
      ($docs | any(.[];
        .kind == "ReferenceGrant"
        and (.metadata.namespace // "default") == $ref.targetNamespace
        and any(.spec.from[]?;
          (.group // "") == $ref.routeGroup
          and .kind == $ref.routeKind
          and .namespace == $ref.sourceNamespace
        )
        and any(.spec.to[]?;
          (.group // "") == $ref.backendGroup
          and .kind == $ref.backendKind
          and ((.name // $ref.backendName) == $ref.backendName)
        )
      )) | not
    )
  | [
      "\(.sourceNamespace)/\(.routeKind)/\(.routeName)",
      "\(.targetNamespace)/\(.backendKind)/\(.backendName)"
    ]
  | @tsv
')"

if [ -n "${uncovered}" ]; then
	echo "cross-namespace backendRefs missing a matching ReferenceGrant:" >&2
	while IFS=$'\t' read -r route backend; do
		printf '  %s -> %s\n' "${route}" "${backend}" >&2
	done <<<"${uncovered}"
	exit 1
fi

echo "ReferenceGrant audit passed (${#manifests[@]} manifest files)"
