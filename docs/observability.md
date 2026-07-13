---
type: Specification
title: Observability Spec
description: Metrics, traces, dashboards, and the LLM token-burn alert across the platform (§9).
---

# Observability Spec (formerly SPEC §9) — metrics and trace backend live

1. **Metrics (§9.1):** kube-prometheus-stack (Prometheus + Alertmanager + Grafana). The agentgateway chart supplies control-plane/proxy monitors and GenAI token, latency, TTFT, and TPOT series; kagent exposes controller `/metrics`; CNPG enables a PodMonitor. License note: Grafana and Loki are AGPL-3.0 (fine to run unmodified, documented as swappable — VictoriaMetrics/VictoriaLogs are the Apache-2.0 alternates for AGPL-banning shops; Prometheus/OTel/Jaeger are Apache-2.0).
1. **Traces (§9.2):** a dedicated OTel Collector accepts OTLP/gRPC (`4317`) and OTLP/HTTP (`4318`) from every workload namespace, batches traces, and exports only traces to Jaeger. Jaeger is the Apache-2.0 default over Tempo (AGPL); its query API has no HTTPRoute and is available to operators through the provisioned Grafana datasource. The bridge emits one content-free span per delegation (queue → A2A send/poll → Matrix reply) and injects W3C `traceparent`; kagent tracing and agentgateway's tracing policy target the same Collector. Do not claim cross-component continuity until the integration test proves one trace covers mention → gateway → agent → LLM.
1. **Bridge metrics and audit (§9.3):** Prometheus `/metrics` on a side port — delegations total/by agent, A2A latency histogram, queue depth, dedup hits, rate-limit rejections. The bridge separately marks content-free `fgentic.delegation.v1` JSON records with `log_stream=audit`; each records terminal reason/duration and explicit dedup/rate-limit verdicts, including suppressed redeliveries. The reference deploys no log database: VictoriaLogs remains a future explicit, pinned, access-controlled per-overlay opt-in and never a bridge runtime dependency. See the [attribution runbook](audit.md).
1. **Declarative dashboards:** Grafana's kube-prometheus-stack sidecar provisions `Fgentic — Bridge` (delegations, latency quantiles, queue/inflight pressure, rate-limit rejections, dedup) and `Fgentic — LLM Token & Cost Guard` (provider/model/route token signals, cost-catalog coverage, and 15-minute burn against the per-cluster guard) from versioned JSON in `infra/observability/dashboards/`. `mise run check:dashboards` parses every panel/query and validates the Flux-rendered ConfigMap. Live rendering and non-empty data remain cluster acceptance checks.
1. **Agent evaluation (§9.4):** MLflow (Apache-2.0, LF) as the optional eval/experiment store, fed by OTel GenAI traces; deployed off by default (own namespace + DB) — it is analysis tooling, not a runtime dependency.
1. **LLM budget (§9.5):** the provider-neutral `system_model_token_type:agentgateway_gen_ai_client_token_usage_sum:increase15m` recording rule preserves provider/model/token-type dimensions, while `LLMTokenBurnHigh` aggregates every profile against the per-cluster `llm_usage_budget_15m` threshold in tokens. This is a token guard, not a currency-cost dashboard. The repository has no versioned model cost catalog, provider billing export, or deterministic model-request-to-task correlation, so exact per-task currency cost is explicitly unavailable; see the [attribution runbook](audit.md).
1. **Self-hosted model health:** the central `vllm` PodMonitor scrapes `/metrics` on the serving engine's internal OpenAI API port. Runtime acceptance requires both an `up` target and non-empty vLLM request metrics after a chat; the agentgateway token histogram remains the provider-neutral budget signal.
1. NetworkPolicies (§9.6) already admit the `monitoring` namespace everywhere (D14).
1. **Database audit (§9.7):** CloudNativePG emits content-suppressed pgAudit `DDL`/`ROLE` records as structured JSON stdout. They remain node-runtime logs today and are an explicit selected stream for the future opt-in log pipeline in #157, not a durable store by themselves.

## 9.2 Trace data plane

```text
workload SDK --OTLP--> otel-collector --OTLP--> Jaeger memory store
                                                    ^
                                                    |
                                      Grafana datasource proxy
```

The reference is intentionally small: one Collector and one Jaeger v2 all-in-one replica, each limited to 256 MiB. Both images and Helm charts are immutable-version pinned, service-account tokens are disabled, containers are non-root with read-only filesystems, and NetworkPolicies allow only the two OTLP ingress ports. Prometheus scrapes their internal telemetry on `8888`. Jaeger retains at most 10,000 traces in memory, so a restart loses trace history by design.

Storage choices are deployment policy, not an application dependency:

1. **Memory (reference default):** no PVC or external service, lowest cost, and disposable history. Use it for local development and the low-cost reference deployment.
1. **Badger + PVC:** survives restarts without another service and suits a single Jaeger all-in-one instance. It cannot scale horizontally; use it only for modest single-node installations and include the PVC in backup policy.
1. **OpenSearch/Elasticsearch or Cassandra:** use an external, persistent backend when retention, replication, or horizontal scale matters. Size and secure it independently; these options materially increase operational cost and are not enabled by the reference.

The Collector-to-Jaeger hop is unencrypted inside the cluster and constrained by NetworkPolicy. A deployment whose threat model does not trust its pod network must enable OTLP TLS/mTLS or a service-mesh identity layer before carrying sensitive span attributes. Prompts and response bodies must not be recorded as span attributes by default.

`mise run test:tracing` renders the pinned charts, starts their exact images in an isolated Docker network, submits a synthetic OTLP span through the Collector, and queries it through the Grafana-provisioned Jaeger datasource. That proves the backend path and datasource contract without claiming cross-component trace continuity. Full acceptance for #35 still requires one installed-cluster mention whose bridge, gateway, agent, and model spans share a trace ID. No Jaeger Ingress or HTTPRoute is enabled.

## 9.7 Database audit stream

The shared CNPG cluster enables pgAudit through four operator-managed parameters: `pgaudit.log=ddl, role`, with catalog-only noise, SQL statement text, and parameters disabled. CloudNativePG manages `shared_preload_libraries` and the extension lifecycle for every connectable database, then emits each parsed record with `logger=pgaudit`, `msg=record`, and typed fields under `record.audit`. Normal Synapse and application `READ`/`WRITE` traffic is not audited, so ordinary platform activity does not become a high-volume content stream.

`scripts/lib/postgres-audit.jq` is the reviewed minimal SQL/payload-suppressed projection for operators and #157: it retains time, pod, database/session role, database, session ID, class, command, statement IDs, and object type/name while dropping statement and parameter fields. Those retained identifiers remain sensitive operational metadata. Pod stdout has no repository-defined retention or access layer. The future log pipeline must select that projection, enforce its own retention/authentication/TLS controls, and keep debug and ordinary PostgreSQL records out; until then, no durable pgAudit claim is made.

`pg_stat_statements` is deliberately deferred. Although CNPG can manage it, query statistics are a distinct performance-observability feature with sizing, access, reset, query-text, and retention decisions; enabling it is neither free nor required to establish the DDL/ROLE audit boundary.
