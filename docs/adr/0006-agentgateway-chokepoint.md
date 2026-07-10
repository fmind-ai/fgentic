# 0006 — agentgateway as the Egress Chokepoint (with honest scope)

Status: Accepted

## Context

Two egress hops carry agent traffic: **bridge → agent** (A2A `message/send`) and **agent → LLM** (the model call the agent itself makes). agentgateway (`v1.3.x`; v1.0.0 shipped 2026-03) is an AI-native data plane with first-class A2A routing (`AgentgatewayBackend{a2a:{host,port}}` + HTTPRoute) and a single model-credential chokepoint for LLM egress. The temptation is to overclaim what routing A2A "through the gateway" actually buys — this ADR records the honest scope so nobody assumes protection that is not there.

## Decision

Route **both** hops through **agentgateway** on `ClusterIP`, wrapped in a `NetworkPolicy` (not internet-exposed):

1. **agent → LLM hop:** agentgateway holds the **single model credential**. Every kagent agent's model calls egress through its OpenAI-compatible endpoint (`http://agentgateway-proxy.agentgateway-system:8080`). This is the real, load-bearing benefit — one credential, one place to rotate, unified LLM/MCP/A2A telemetry.
1. **bridge → agent hop:** the A2A call is routed to agentgateway's A2A backend, which provides **telemetry (JSON-RPC method logging) + agent-card URL rewriting** — **not** response inspection and **not** A2A-level authorization.

Because the bridge→agent hop gives telemetry but not authz, hitting kagent's A2A endpoint (`http://kagent-controller.kagent:8083/api/a2a/<ns>/<name>`) directly is **functionally equivalent** for fire-and-forget. Routing A2A through the gateway is therefore an explicit **observability/governance choice** (valuable for the showcase), **toggleable in the bridge config**. Who-may-invoke-which-agent authorization lives in the **bridge allowlist** ([ADR 0005](0005-matrix-a2a-bridge-appservice.md)), not in the gateway.

## Consequences

1. The model credential lives in exactly one place; no agent Deployment ever holds it — the central, decisive win.
1. The A2A-through-gateway hop is honest about scope: unified telemetry and agent-card URL rewriting, not a security boundary — so the bridge allowlist remains the authorization control.
1. The A2A route is a toggle: a cost-constrained or debugging deployment can point the bridge straight at `kagent-controller:8083` with no loss of correctness, only of centralised telemetry.
1. Everything stays on ClusterIP + NetworkPolicy — agent traffic never touches Traefik or the public internet ([ADR 0002](0002-matrix-collaboration-fabric.md) reserves Traefik for human web/UI traffic).
