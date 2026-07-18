#!/usr/bin/env bash
# Render the production-pinned tracing charts, run their exact images/configs in an isolated
# Docker network, submit one OTLP/HTTP span through the Collector, and query it from Jaeger.
# This is the constrained-host fallback for environments where kind cannot bootstrap.
set -euo pipefail

tmp_dir="$(mktemp -d)"
suffix="$$"
network="fgentic-tracing-${suffix}"
jaeger_container="fgentic-jaeger-${suffix}"
collector_container="fgentic-otel-${suffix}"
grafana_container="fgentic-grafana-${suffix}"

cleanup() {
	docker rm -f "${grafana_container}" "${collector_container}" "${jaeger_container}" >/dev/null 2>&1 || true
	docker network rm "${network}" >/dev/null 2>&1 || true
	rm -rf "${tmp_dir}"
}
trap cleanup EXIT

dump_logs() {
	docker logs "${jaeger_container}" 2>&1 || true
	docker logs "${collector_container}" 2>&1 || true
	docker logs "${grafana_container}" 2>&1 || true
}

fail() {
	echo "tracing acceptance failed: $*" >&2
	dump_logs >&2
	exit 1
}

render_release() {
	local release="$1"
	local output="$2"
	local chart version source repository
	chart="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.chart" infra/observability/tracing-helmreleases.yaml)"
	version="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.version" infra/observability/tracing-helmreleases.yaml)"
	source="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.sourceRef.name" infra/observability/tracing-helmreleases.yaml)"
	repository="$(yq -er "select(.kind == \"HelmRepository\" and .metadata.name == \"${source}\") | .spec.url" infra/flux/sources.yaml)"
	yq -e "select(.metadata.name == \"${release}\") | .spec.values" infra/observability/tracing-helmreleases.yaml \
		| flux envsubst --strict \
		| helm template "${release}" "${chart}" \
			--repo "${repository}" \
			--version "${version}" \
			--namespace monitoring \
			--values - >"${output}"
}

command -v docker >/dev/null || fail "docker is required"
docker info >/dev/null 2>&1 || fail "Docker daemon is not available"

datasource_url="$(yq -er '.spec.values.grafana.additionalDataSources[] | select(.uid == "jaeger") | .url' infra/observability/helmrelease.yaml)"
[[ "${datasource_url}" == "http://jaeger.monitoring.svc.cluster.local:16686" ]] \
	|| fail "Grafana Jaeger datasource does not target the in-cluster query service"
yq -e 'select(.metadata.name == "jaeger") | .spec.values.jaeger.ingress.enabled == false and .spec.values.jaeger.httproute.enabled == false' \
	infra/observability/tracing-helmreleases.yaml >/dev/null \
	|| fail "Jaeger must not expose an Ingress or HTTPRoute"

echo "==> Rendering pinned OpenTelemetry and Jaeger charts"
render_release otel-collector "${tmp_dir}/otel.yaml"
render_release jaeger "${tmp_dir}/jaeger.yaml"

prometheus_chart="$(yq -er '.spec.chart.spec.chart' infra/observability/helmrelease.yaml)"
prometheus_version="$(yq -er '.spec.chart.spec.version' infra/observability/helmrelease.yaml)"
prometheus_source="$(yq -er '.spec.chart.spec.sourceRef.name' infra/observability/helmrelease.yaml)"
prometheus_repository="$(yq -er "select(.kind == \"HelmRepository\" and .metadata.name == \"${prometheus_source}\") | .spec.url" infra/flux/sources.yaml)"
grafana_image="$(
	export server_name=tracing.test
	yq -e '.spec.values' infra/observability/helmrelease.yaml \
		| flux envsubst --strict \
		| helm template kube-prometheus-stack "${prometheus_chart}" \
			--repo "${prometheus_repository}" \
			--version "${prometheus_version}" \
			--namespace monitoring \
			--values - \
		| yq -er 'select(.kind == "Deployment" and .metadata.name == "kube-prometheus-stack-grafana") | .spec.template.spec.containers[] | select(.name == "grafana") | .image'
)"
yq -o=yaml \
	'{"apiVersion": 1, "datasources": [.spec.values.grafana.additionalDataSources[] | select(.uid == "jaeger")]}' \
	infra/observability/helmrelease.yaml >"${tmp_dir}/grafana-datasources.yaml"

yq -r 'select(.kind == "ConfigMap" and .metadata.name == "otel-collector") | .data.relay' \
	"${tmp_dir}/otel.yaml" >"${tmp_dir}/otel-config.yaml"
yq -r 'select(.kind == "ConfigMap" and .metadata.name == "user-config") | .data."user-config.yaml"' \
	"${tmp_dir}/jaeger.yaml" >"${tmp_dir}/jaeger-config.yaml"
collector_image="$(yq -er 'select(.kind == "Deployment" and .metadata.name == "otel-collector") | .spec.template.spec.containers[0].image' "${tmp_dir}/otel.yaml")"
jaeger_image="$(yq -er 'select(.kind == "Deployment" and .metadata.name == "jaeger") | .spec.template.spec.containers[0].image' "${tmp_dir}/jaeger.yaml")"

echo "==> Starting the exact pinned images in an isolated Docker network"
docker network create "${network}" >/dev/null
docker run --pull missing -d --name "${jaeger_container}" --network "${network}" \
	--network-alias jaeger.monitoring.svc.cluster.local \
	--read-only --cap-drop ALL --security-opt no-new-privileges --memory 256m \
	-p 127.0.0.1::16686 \
	-v "${tmp_dir}/jaeger-config.yaml:/etc/jaeger/user-config.yaml:ro" \
	"${jaeger_image}" --config /etc/jaeger/user-config.yaml >/dev/null
docker run --pull missing -d --name "${collector_container}" --network "${network}" \
	--read-only --cap-drop ALL --security-opt no-new-privileges --memory 256m \
	-e MY_POD_IP=0.0.0.0 \
	-p 127.0.0.1::4318 -p 127.0.0.1::13133 \
	-v "${tmp_dir}/otel-config.yaml:/etc/otelcol-k8s/config.yaml:ro" \
	"${collector_image}" --config=/etc/otelcol-k8s/config.yaml >/dev/null
docker run --pull missing -d --name "${grafana_container}" --network "${network}" \
	--read-only --cap-drop ALL --security-opt no-new-privileges --memory 512m \
	--tmpfs /var/lib/grafana:uid=472,gid=0 --tmpfs /var/log/grafana:uid=472,gid=0 \
	-e GF_SECURITY_ADMIN_USER=admin -e GF_SECURITY_ADMIN_PASSWORD=fgentic-local-test \
	-e GF_USERS_ALLOW_SIGN_UP=false \
	-p 127.0.0.1::3000 \
	-v "${tmp_dir}/grafana-datasources.yaml:/etc/grafana/provisioning/datasources/jaeger.yaml:ro" \
	"${grafana_image}" >/dev/null

jaeger_port="$(docker port "${jaeger_container}" 16686/tcp | sed 's/.*://')"
collector_port="$(docker port "${collector_container}" 4318/tcp | sed 's/.*://')"
collector_health_port="$(docker port "${collector_container}" 13133/tcp | sed 's/.*://')"
grafana_port="$(docker port "${grafana_container}" 3000/tcp | sed 's/.*://')"
for attempt in {1..120}; do
	jaeger_ready=false
	collector_ready=false
	grafana_ready=false
	xh --timeout 2 --check-status --ignore-stdin "http://127.0.0.1:${jaeger_port}/api/services" >/dev/null 2>&1 \
		&& jaeger_ready=true
	xh --timeout 2 --check-status --ignore-stdin "http://127.0.0.1:${collector_health_port}/" >/dev/null 2>&1 \
		&& collector_ready=true
	xh --timeout 2 --check-status --ignore-stdin "http://127.0.0.1:${grafana_port}/api/health" >/dev/null 2>&1 \
		&& grafana_ready=true
	if ${jaeger_ready} && ${collector_ready} && ${grafana_ready}; then
		break
	fi
	[[ "${attempt}" -lt 120 ]] || fail "Collector, Jaeger, or Grafana did not become ready"
	sleep 1
done

start="$(($(date +%s) * 1000000000))"
end="$((start + 1000000))"
trace_id=5b8efff798038103d269b633813fc60c
payload="{\"resourceSpans\":[{\"resource\":{\"attributes\":[{\"key\":\"service.name\",\"value\":{\"stringValue\":\"fgentic-tracing-acceptance\"}}]},\"scopeSpans\":[{\"scope\":{\"name\":\"fgentic.local-test\"},\"spans\":[{\"traceId\":\"${trace_id}\",\"spanId\":\"eee19b7ec3c1b174\",\"name\":\"collector-to-jaeger\",\"kind\":1,\"startTimeUnixNano\":\"${start}\",\"endTimeUnixNano\":\"${end}\",\"status\":{\"code\":1}}]}]}]}"
echo "==> Sending a synthetic OTLP span through the Collector"
xh --timeout 5 --check-status POST "http://127.0.0.1:${collector_port}/v1/traces" \
	Content-Type:application/json <<<"${payload}" >/dev/null

for attempt in {1..30}; do
	response="$(xh --timeout 5 --check-status --ignore-stdin \
		-a admin:fgentic-local-test \
		"http://127.0.0.1:${grafana_port}/api/datasources/proxy/uid/jaeger/api/traces?service=fgentic-tracing-acceptance")"
	if jq -e --arg trace_id "${trace_id}" \
		'.data | length == 1 and .[0].traceID == $trace_id and any(.[0].spans[]; .operationName == "collector-to-jaeger")' \
		<<<"${response}" >/dev/null; then
		docker logs "${collector_container}" 2>&1 \
			| rg -q 'legacy service.telemetry.resource|alias is deprecated' \
			&& fail "Collector emitted a deprecated-config warning"
		docker logs "${jaeger_container}" 2>&1 \
			| rg -q 'legacy service.telemetry.resource' \
			&& fail "Jaeger emitted a deprecated-config warning"
		echo "OTLP acceptance passed: trace ${trace_id} reached Grafana through Collector and Jaeger"
		exit 0
	fi
	[[ "${attempt}" -lt 30 ]] || fail "trace was not queryable from Jaeger"
	sleep 1
done
