# Observability Spec (formerly SPEC §9) — metrics and trace backend live

1. **Metrics:** kube-prometheus-stack (Prometheus + Alertmanager + Grafana). agentgateway ships ServiceMonitors + a Grafana dashboard (control plane and per-proxy GenAI metrics: `gen_ai_client_token_usage`, `gen_ai_client_cost`, TTFT/TPOT); kagent exposes authenticated controller `/metrics`; CNPG enables `monitoring.enablePodMonitor` (stub already commented in `cluster.yaml`, gated on the Prometheus Operator CRDs). License note: Grafana and Loki are AGPL-3.0 (fine to run unmodified, documented as swappable — VictoriaMetrics/VictoriaLogs are the Apache-2.0 alternates for AGPL-banning shops; Prometheus/OTel/Jaeger are Apache-2.0).
1. **Traces:** a dedicated OTel Collector accepts OTLP/gRPC (`4317`) and OTLP/HTTP (`4318`) from every workload namespace, batches traces, and exports only traces to Jaeger. Jaeger is the Apache-2.0 default over Tempo (AGPL); its query API has no HTTPRoute and is available to operators through the provisioned Grafana datasource. Application instrumentation remains explicit: enable kagent `otel.tracing`, configure agentgateway OTLP export, and instrument the bridge with a span per delegation (Matrix event → A2A send → poll → reply post) while propagating W3C `traceparent`. Do not claim cross-component continuity until the integration test proves one trace covers mention → gateway → agent → LLM.
1. **Bridge metrics:** Prometheus `/metrics` on a side port — delegations total/by agent, A2A latency histogram, queue depth, dedup hits, rate-limit rejections. Also structured audit logs (who invoked what, from which room/server).
1. **Agent evaluation:** MLflow (Apache-2.0, LF) as the optional eval/experiment store, fed by OTel GenAI traces; deployed off by default (own namespace + DB) — it is analysis tooling, not a runtime dependency.
1. **LLM cost:** agentgateway cost catalogs + `gen_ai_client_cost` are the budget dashboard — wire an Alertmanager rule on spend rate (the failure mode that killed the closest prior-art project).
1. NetworkPolicies already admit the `monitoring` namespace everywhere (D14).

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

For local acceptance, render both pinned charts, run their exact image digests in an isolated Docker network, submit a synthetic OTLP span through the Collector, and query it through Jaeger's datasource API. Grafana provisioning is considered valid only when the rendered `Jaeger` datasource points to the cluster-internal query service and no Jaeger Ingress or HTTPRoute is enabled.
