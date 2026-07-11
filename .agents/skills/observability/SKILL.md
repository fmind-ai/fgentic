---
name: observability
description: Operate and extend Fgentic's metrics layer — Grafana/Prometheus access, the key metric names (delegations, GenAI tokens), the LLM spend alert, and how to add monitors and rules. Use when investigating platform behavior/cost or adding observability for a new component.
metadata:
  author: Médéric Hurier (Fmind)
  created: 2026-07-11
---

# Observability

kube-prometheus-stack lives in ns `monitoring` (`infra/observability/`); the platform-specific monitors and rules are the **dependent** `observability-monitors` layer (`infra/observability/monitors/` — split because PodMonitor/PrometheusRule CRDs must exist first). Spec: SPEC §9 → [docs/observability.md](../../../docs/observability.md). Traces & audit are M5 — not built yet.

## Access

1. Grafana: `https://grafana.<server_name>` — admin password: `kubectl -n monitoring get secret kube-prometheus-stack-grafana -o jsonpath='{.data.admin-password}' | base64 -d`.
1. Ad-hoc PromQL without the UI: `kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090` then query `http://localhost:9090`.

## Key metrics (names verified live)

1. **Bridge**: `fgentic_delegations_total` (+ the rest of the bridge's Prometheus side-port) — delegation volume, outcomes, rate-limit hits.
1. **agentgateway GenAI**: prefixed `agentgateway_gen_ai_*`; token metering is `agentgateway_gen_ai_client_token_usage_sum` — the platform's cost signal, labeled per model/route.
1. **Cost alert**: `LLMTokenBurnHigh` (`infra/observability/monitors/cost-alert.yaml`) fires on sustained token burn (>100k/15m default, configured by `llm_usage_budget_15m` in platform-settings). `system_model_token_type:agentgateway_gen_ai_client_token_usage_sum:increase15m` keeps the provider/model/token-type breakdown for dashboards. Cost is the #1 failure mode (D7/D8): if you change rate limits or add automation that can invoke agents, check this alert still bounds the blast radius and run `mise run check:prometheus`.

## Investigating

1. "Is the platform being used / abused?" — delegations rate by room/sender (bridge metrics) vs token usage (agentgateway): a token spike without matching delegations means something bypasses the bridge path; a mention storm shows in both plus rate-limit counters.
1. Scrape target missing? `kubectl -n monitoring get podmonitors,servicemonitors -A` and check Prometheus's Targets page — the usual cause is a label selector/port-name mismatch in `monitors/podmonitors.yaml`.

## Extending

1. New component to scrape → add a PodMonitor to `infra/observability/monitors/` (it rides the existing Kustomization; commit, Flux reconciles). New alert → a PrometheusRule in the same layer, with a comment citing the SPEC § and decision it enforces.
1. Expose metrics on a **separate side port** (the bridge pattern) so the metrics endpoint never routes through the Gateway.
1. Dashboards are Grafana-native for now; anything worth keeping should be provisioned declaratively via the chart values (`infra/observability/helmrelease.yaml`), not hand-saved in the UI.
