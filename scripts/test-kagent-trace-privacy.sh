#!/usr/bin/env bash
# Run the exact pinned kagent Python runtime against local model/tool fixtures and prove that
# producer-side privacy controls remove content before the production Collector and Jaeger path.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly KAGENT_VERSION="0.9.11"
# Multi-architecture app digest linked into the pinned kagent 0.9.11 controller build.
readonly KAGENT_IMAGE="cr.kagent.dev/kagent-dev/kagent/app@sha256:e2d28b8f4dfc49364590b7de48effeda2627dec9cb89d170ac10839f0cfe712d"
readonly COLLECTOR_IMAGE="ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-k8s@sha256:4bb87d79b9f4f7971cf0c7c2fbf7decab3ddfda0a8f73794e98d9bc6fe77f609"
readonly JAEGER_IMAGE="jaegertracing/jaeger@sha256:ede4864215be4cd85bd8c3129a2fea6c5713c5653c7282c429dba123014bc68b"

tmp_dir="$(mktemp -d)"
suffix="$$"
network="fgentic-kagent-trace-privacy-${suffix}"
fixture_container="fgentic-kagent-trace-fixture-${suffix}"
collector_container="fgentic-kagent-trace-collector-${suffix}"
jaeger_container="fgentic-kagent-trace-jaeger-${suffix}"

cleanup() {
  docker rm -f \
    "fgentic-kagent-trace-runner-leak-${suffix}" \
    "fgentic-kagent-trace-runner-safe-${suffix}" \
    "${fixture_container}" \
    "${collector_container}" \
    "${jaeger_container}" >/dev/null 2>&1 || true
  docker network rm "${network}" >/dev/null 2>&1 || true
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

dump_logs() {
  local container log
  for container in "${fixture_container}" "${collector_container}" "${jaeger_container}"; do
    if docker inspect "${container}" >/dev/null 2>&1; then
      docker logs "${container}" 2>&1 || true
    fi
  done
  for log in "${tmp_dir}"/runner-*.log; do
    [[ -f "${log}" ]] && cat "${log}" || true
  done
}

fail() {
  echo "kagent trace privacy acceptance failed: $*" >&2
  dump_logs >&2
  exit 1
}

require_commands() {
  local command
  for command in "$@"; do
    command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
  done
}

render_release() {
  local release="$1"
  local output="$2"
  local chart version source repository
  chart="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.chart" \
    "${ROOT_DIR}/infra/observability/tracing-helmreleases.yaml")"
  version="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.version" \
    "${ROOT_DIR}/infra/observability/tracing-helmreleases.yaml")"
  source="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.sourceRef.name" \
    "${ROOT_DIR}/infra/observability/tracing-helmreleases.yaml")"
  repository="$(yq -er "select(.kind == \"HelmRepository\" and .metadata.name == \"${source}\") | .spec.url" \
    "${ROOT_DIR}/infra/flux/sources.yaml")"
  yq -e "select(.metadata.name == \"${release}\") | .spec.values" \
    "${ROOT_DIR}/infra/observability/tracing-helmreleases.yaml" \
    | flux envsubst --strict \
    | helm template "${release}" "${chart}" \
      --repo "${repository}" \
      --version "${version}" \
      --namespace monitoring \
      --values - >"${output}"
}

query_trace() {
  local service="$1"
  local output="$2"
  local response
  for attempt in {1..40}; do
    if response="$(docker exec "${fixture_container}" python -c '
import sys
import urllib.parse
import urllib.request
url = "http://jaeger:16686/api/traces?" + urllib.parse.urlencode({"service": sys.argv[1]})
print(urllib.request.urlopen(url, timeout=3).read().decode())
' "${service}" 2>/dev/null)" \
      && jq -e '(.data | length) == 1' <<<"${response}" >/dev/null; then
      printf '%s' "${response}" >"${output}"
      return
    fi
    [[ "${attempt}" -lt 40 ]] || fail "trace for ${service} was not queryable from Jaeger"
    sleep 0.25
  done
}

run_agent() {
  local label="$1"
  shift
  if ! timeout --foreground --signal=TERM --kill-after=10s 180s \
    docker run --rm --name "fgentic-kagent-trace-runner-${label,,}-${suffix}" \
    --network "${network}" \
    --entrypoint python \
    --read-only --cap-drop ALL --security-opt no-new-privileges --memory 512m \
    --tmpfs /tmp:rw,nosuid,nodev,noexec,size=64m \
    -e HOME=/tmp \
    -e OPENAI_API_KEY=fixture-not-a-secret \
    -e OTEL_TRACING_ENABLED=true \
    -e OTEL_LOGGING_ENABLED=false \
    -e OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4317 \
    -e OTEL_EXPORTER_OTLP_PROTOCOL=grpc \
    -e OTEL_EXPORTER_OTLP_INSECURE=true \
    "$@" \
    -v "${tmp_dir}/runner.py:/runner.py:ro" \
    "${KAGENT_IMAGE}" /runner.py "${label}" \
    >"${tmp_dir}/runner-${label,,}.log" 2>&1; then
    fail "${label} kagent run failed"
  fi
}

require_commands docker flux helm jq rg timeout yq
docker info >/dev/null 2>&1 || fail "Docker daemon is not available"

actual_kagent_version="$(yq -er 'select(.metadata.name == "kagent") | .spec.chart.spec.version' \
  "${ROOT_DIR}/infra/kagent/helmrelease.yaml")"
[[ "${actual_kagent_version}" == "${KAGENT_VERSION}" ]] \
  || fail "kagent chart is ${actual_kagent_version}; review and update the runtime privacy digest"

echo "==> Checking producer and forwarding contracts"
yq -o=json -I=0 \
  'select(.kind == "AgentgatewayPolicy" and .metadata.name == "tracing") | .spec.frontend.tracing' \
  "${ROOT_DIR}/infra/agentgateway/base/tracing-policy.yaml" >"${tmp_dir}/gateway-tracing.json"
jq -e '
  (keys | sort) == ["backendRef", "clientSampling", "protocol", "randomSampling", "resources"] and
  .clientSampling == "true" and .randomSampling == "true" and
  .protocol == "GRPC" and
  .backendRef == {"name":"otel-collector", "namespace":"monitoring", "port":4317} and
  .resources == [{"name":"service.name", "expression":"\"agentgateway\""}]
' "${tmp_dir}/gateway-tracing.json" >/dev/null \
  || fail "agentgateway tracing policy added an unreviewed field or resource expression"

yq -o=json -I=0 \
  'select(.metadata.name == "otel-collector") | .spec.values.alternateConfig' \
  "${ROOT_DIR}/infra/observability/tracing-helmreleases.yaml" >"${tmp_dir}/collector-source.json"
jq -e '
  (.service.pipelines | keys) == ["traces"] and
  (.processors | keys | sort) == ["batch", "memory_limiter"] and
  (.receivers | keys) == ["otlp"] and
  (.exporters | keys) == ["otlp_grpc/jaeger"] and
  .service.pipelines.traces.receivers == ["otlp"] and
  .service.pipelines.traces.processors == ["memory_limiter", "batch"] and
  .service.pipelines.traces.exporters == ["otlp_grpc/jaeger"]
' "${tmp_dir}/collector-source.json" >/dev/null \
  || fail "Collector must remain a pass-through OTLP traces pipeline without logs or enrichment"

for agent in docs-qa platform-helper scribe; do
  yq eval-all -e \
    "select(.kind == \"Agent\" and .metadata.name == \"${agent}\" and
      (.spec.declarative.deployment.env | length) == 3 and
      ([.spec.declarative.deployment.env[] | select(.value == \"false\" and (has(\"valueFrom\") | not))] | length) == 3 and
      ([.spec.declarative.deployment.env[].name] | sort | join(\",\")) ==
        \"ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS,OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT,TRACELOOP_TRACE_CONTENT\")" \
    "${ROOT_DIR}/infra/kagent/agent-zoo.yaml" >/dev/null \
    || fail "${agent} does not carry the exact literal trace-content controls"
done
yq eval-all -N -r '
  select(.kind == "Agent" and .metadata.name == "platform-helper") |
  .spec.declarative.deployment.env[] | .name + "=" + .value
' "${ROOT_DIR}/infra/kagent/agent-zoo.yaml" >"${tmp_dir}/safe.env"

cat >"${tmp_dir}/fixture.py" <<'PY'
from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, cast

from mcp.server.fastmcp import FastMCP


def run_label(payload: dict[str, Any]) -> str:
    serialized = json.dumps(payload, separators=(",", ":"))
    for label in ("SAFE", "LEAK"):
        if f"PROMPT_SENTINEL_{label}" in serialized:
            return label
    raise ValueError("missing run sentinel")


class ModelHandler(BaseHTTPRequestHandler):
    def log_message(self, _format: str, *_args: object) -> None:
        return

    def write_json(self, status: int, payload: dict[str, Any]) -> None:
        body = json.dumps(payload, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        if self.path == "/healthz":
            self.write_json(200, {"status": "ok"})
            return
        self.write_json(404, {"error": {"message": "not found"}})

    def do_POST(self) -> None:
        if self.path != "/v1/chat/completions":
            self.write_json(404, {"error": {"message": "not found"}})
            return
        size = int(self.headers.get("Content-Length", "0"))
        payload = cast(dict[str, Any], json.loads(self.rfile.read(size)))
        label = run_label(payload)
        messages = cast(list[dict[str, Any]], payload.get("messages", []))
        has_tool_result = any(message.get("role") == "tool" for message in messages)
        model = cast(str, payload.get("model", "gpt-canary"))
        if has_tool_result:
            message: dict[str, Any] = {
                "role": "assistant",
                "content": f"COMPLETION_SENTINEL_{label}",
            }
            finish_reason = "stop"
        else:
            message = {
                "role": "assistant",
                "content": None,
                "tool_calls": [
                    {
                        "id": f"call_{label.lower()}",
                        "type": "function",
                        "function": {
                            "name": "echo_secret",
                            "arguments": json.dumps(
                                {"secret": f"TOOL_ARG_SENTINEL_{label}"},
                                separators=(",", ":"),
                            ),
                        },
                    }
                ],
            }
            finish_reason = "tool_calls"
        self.write_json(
            200,
            {
                "id": f"chatcmpl-{label.lower()}",
                "object": "chat.completion",
                "created": 0,
                "model": model,
                "choices": [
                    {
                        "index": 0,
                        "message": message,
                        "finish_reason": finish_reason,
                    }
                ],
                "usage": {
                    "prompt_tokens": 11,
                    "completion_tokens": 7,
                    "total_tokens": 18,
                },
            },
        )


mcp = FastMCP(
    "privacy-canary",
    host="0.0.0.0",
    port=8084,
    stateless_http=True,
    json_response=True,
)


@mcp.tool(description="Return a deterministic fixture result.")
def echo_secret(secret: str) -> str:
    """Return a deterministic result for one fixture argument."""
    label = secret.rsplit("_", maxsplit=1)[-1]
    return f"TOOL_RESULT_SENTINEL_{label}"


threading.Thread(
    target=ThreadingHTTPServer(("0.0.0.0", 8081), ModelHandler).serve_forever,
    daemon=True,
).start()
mcp.run(transport="streamable-http")
PY

cat >"${tmp_dir}/runner.py" <<'PY'
from __future__ import annotations

import asyncio
import sys

from google.adk.artifacts import InMemoryArtifactService
from google.adk.runners import Runner
from google.adk.sessions import InMemorySessionService
from google.genai import types
from kagent.adk import AgentConfig
from kagent.core import configure_tracing
from opentelemetry import trace


label = sys.argv[1].upper()
if label not in {"LEAK", "SAFE"}:
    raise ValueError("run label must be LEAK or SAFE")

agent_config = AgentConfig.model_validate(
    {
        "model": {
            "type": "openai",
            "model": "gpt-canary",
            "base_url": "http://fixture:8081/v1",
        },
        "description": "Deterministic privacy canary",
        "instruction": "Call echo_secret exactly once, then return the model completion.",
        "http_tools": [
            {
                "params": {"url": "http://fixture:8084/mcp", "timeout": 10},
                "tools": ["echo_secret"],
            }
        ],
        "stream": False,
    }
)


async def run_canary() -> None:
    app_name = "fgentic_privacy_canary"
    session_id = f"session_{label.lower()}"
    user_id = "fixture_user"
    session_service = InMemorySessionService()
    await session_service.create_session(
        app_name=app_name,
        session_id=session_id,
        user_id=user_id,
    )
    root_agent = agent_config.to_agent("test_agent")
    runner = Runner(
        agent=root_agent,
        app_name=app_name,
        session_service=session_service,
        artifact_service=InMemoryArtifactService(),
    )
    message = types.Content(
        role="user",
        parts=[types.Part(text=f"PROMPT_SENTINEL_{label}: run the fixture.")],
    )
    try:
        async for _event in runner.run_async(
            user_id=user_id,
            session_id=session_id,
            new_message=message,
        ):
            pass
    finally:
        for tool in root_agent.tools:
            close = getattr(tool, "close", None)
            if close is not None:
                await close()


configure_tracing(f"privacy_canary_{label.lower()}", "fgentic_test")
asyncio.run(run_canary())
provider = trace.get_tracer_provider()
if not provider.force_flush(timeout_millis=10_000):
    raise RuntimeError("trace provider did not flush")
provider.shutdown()
PY

echo "==> Rendering and starting the exact production trace path"
render_release otel-collector "${tmp_dir}/otel.yaml"
render_release jaeger "${tmp_dir}/jaeger.yaml"
yq -r 'select(.kind == "ConfigMap" and .metadata.name == "otel-collector") | .data.relay' \
  "${tmp_dir}/otel.yaml" >"${tmp_dir}/otel-config.yaml"
yq -r 'select(.kind == "ConfigMap" and .metadata.name == "user-config") | .data."user-config.yaml"' \
  "${tmp_dir}/jaeger.yaml" >"${tmp_dir}/jaeger-config.yaml"
collector_image="$(yq -er 'select(.kind == "Deployment" and .metadata.name == "otel-collector") | .spec.template.spec.containers[0].image' "${tmp_dir}/otel.yaml")"
jaeger_image="$(yq -er 'select(.kind == "Deployment" and .metadata.name == "jaeger") | .spec.template.spec.containers[0].image' "${tmp_dir}/jaeger.yaml")"
[[ "${collector_image}" == "${COLLECTOR_IMAGE}" ]] \
  || fail "rendered Collector image no longer matches the reviewed digest"
[[ "${jaeger_image}" == "${JAEGER_IMAGE}" ]] \
  || fail "rendered Jaeger image no longer matches the reviewed digest"
docker manifest inspect "${KAGENT_IMAGE}" \
  | jq -e '[.manifests[].platform | select(.os == "linux") | .architecture] | sort == ["amd64", "arm64"]' \
    >/dev/null \
  || fail "kagent runtime index must contain exactly linux/amd64 and linux/arm64"

docker pull "${KAGENT_IMAGE}" >/dev/null
docker pull "${collector_image}" >/dev/null
docker pull "${jaeger_image}" >/dev/null
docker network create --internal "${network}" >/dev/null
docker run -d --name "${jaeger_container}" --network "${network}" \
  --network-alias jaeger \
  --network-alias jaeger.monitoring.svc.cluster.local \
  --read-only --cap-drop ALL --security-opt no-new-privileges --memory 256m \
  -v "${tmp_dir}/jaeger-config.yaml:/etc/jaeger/user-config.yaml:ro" \
  "${jaeger_image}" --config /etc/jaeger/user-config.yaml >/dev/null
docker run -d --name "${collector_container}" --network "${network}" \
  --network-alias collector \
  --read-only --cap-drop ALL --security-opt no-new-privileges --memory 256m \
  -e MY_POD_IP=0.0.0.0 \
  -v "${tmp_dir}/otel-config.yaml:/etc/otelcol-k8s/config.yaml:ro" \
  "${collector_image}" --config=/etc/otelcol-k8s/config.yaml >/dev/null
docker run -d --name "${fixture_container}" --network "${network}" \
  --network-alias fixture --entrypoint python \
  -v "${tmp_dir}/fixture.py:/fixture.py:ro" \
  "${KAGENT_IMAGE}" /fixture.py >/dev/null

for attempt in {1..80}; do
  if docker exec "${fixture_container}" python -c '
import urllib.request
import socket
for url in (
    "http://127.0.0.1:8081/healthz",
    "http://collector:13133/",
    "http://jaeger:16686/api/services",
):
    urllib.request.urlopen(url, timeout=2).read()
socket.create_connection(("127.0.0.1", 8084), timeout=2).close()
' >/dev/null 2>&1; then
    break
  fi
  [[ "${attempt}" -lt 80 ]] || fail "fixture, Collector, or Jaeger did not become ready"
  sleep 0.25
done

echo "==> Proving the positive control can expose every content class"
run_agent LEAK \
  -e ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS=true \
  -e OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true \
  -e TRACELOOP_TRACE_CONTENT=true
query_trace privacy_canary_leak "${tmp_dir}/leak.json"
for kind in PROMPT COMPLETION TOOL_ARG TOOL_RESULT; do
  rg -Fq "${kind}_SENTINEL_LEAK" "${tmp_dir}/leak.json" \
    || fail "positive-control trace did not expose ${kind} content"
done

echo "==> Proving the managed Agent controls suppress content"
run_agent SAFE --env-file "${tmp_dir}/safe.env"
query_trace privacy_canary_safe "${tmp_dir}/safe.json"
for kind in PROMPT COMPLETION TOOL_ARG TOOL_RESULT; do
  if rg -Fq "${kind}_SENTINEL_SAFE" "${tmp_dir}/safe.json"; then
    fail "protected trace exposed ${kind} content"
  fi
done

jq -e '
  . as $root |
  (.data | length) == 1 and
  (.data[0].traceID | test("^[0-9a-f]{32}$") and . != "00000000000000000000000000000000") and
  (.data[0].spans | length) >= 10 and
  ([.data[0].spans[].traceID] | unique) == [.data[0].traceID] and
  (all(.data[0].spans[]; (.spanID | test("^[0-9a-f]{16}$") and . != "0000000000000000"))) and
  ([.data[0].spans[].operationName] as $operations |
    all(["invoke_agent test_agent", "generate_content gpt-canary", "call_llm", "openai.chat", "execute_tool echo_secret"][];
      . as $required | $operations | index($required))) and
  any(.data[0].spans[].tags[]?; .key == "gen_ai.request.model" and .value == "gpt-canary") and
  any(.data[0].spans[].tags[]?; .key == "gen_ai.system" and .value == "openai") and
  any(.data[0].spans[].tags[]?; .key == "gen_ai.tool.name" and .value == "echo_secret") and
  any(.data[0].spans[].tags[]?; .key == "gen_ai.usage.input_tokens" and .value == 11) and
  any(.data[0].spans[].tags[]?; .key == "gen_ai.usage.output_tokens" and .value == 7) and
  all(.data[0].spans[].tags[]?; (.key | test("^gen_ai\\.(prompt|completion)\\.") | not)) and
  (["gcp.vertex.agent.llm_request", "gcp.vertex.agent.llm_response", "gcp.vertex.agent.tool_call_args", "gcp.vertex.agent.tool_response"] as $keys |
    all($keys[]; . as $key |
      ([$root.data[0].spans[].tags[]? | select(.key == $key)] | length) > 0 and
      all($root.data[0].spans[].tags[]? | select(.key == $key); .value == "{}")))
' "${tmp_dir}/safe.json" >/dev/null \
  || fail "protected trace lost required identity, component, model, token, tool, or empty-content evidence"

trace_id="$(jq -r '.data[0].traceID' "${tmp_dir}/safe.json")"
span_count="$(jq -r '.data[0].spans | length' "${tmp_dir}/safe.json")"
echo "kagent trace privacy acceptance passed: trace ${trace_id}, ${span_count} spans, all sentinels suppressed"
