---
type: Architecture Decision Record
title: agentgateway as the Egress Chokepoint
description: Route all local LLM egress through agentgateway so no agent holds a model credential.
---

# 0006 — agentgateway as the Egress Chokepoint (with honest scope)

Status: Accepted

Amendment: [ADR 0017](0017-permission-aware-identity-binding.md) makes the gateway identity-projection path mandatory only for permission-aware retrieval; the ordinary A2A and model-credential decisions below remain accepted.

## Context

Two egress hops carry agent traffic: **bridge → agent** (A2A `message/send`) and **agent → LLM** (the model call the agent itself makes). agentgateway (`v1.3.x`; v1.0.0 shipped 2026-03) is an AI-native data plane with first-class A2A routing (`AgentgatewayBackend{a2a:{host,port}}` + HTTPRoute) and a single model-credential chokepoint for LLM egress. The temptation is to overclaim what routing A2A "through the gateway" actually buys — this ADR records the honest scope so nobody assumes protection that is not there.

## Decision

Route **both** hops through **agentgateway** on `ClusterIP`, wrapped in a `NetworkPolicy` (not internet-exposed):

1. **agent → LLM hop:** agentgateway holds the **single model credential**. Every kagent agent's model calls egress through its OpenAI-compatible endpoint (`http://agentgateway-proxy.agentgateway-system:8080`). This is the real, load-bearing benefit — one credential, one place to rotate, unified LLM/MCP/A2A telemetry.
1. **bridge → ordinary agent hop:** the A2A call is routed to agentgateway's A2A backend, which provides **telemetry (JSON-RPC method logging) + agent-card URL rewriting** — **not** response inspection and **not** A2A-level authorization. Permission-aware retrieval is the narrow exception added by [ADR 0017](0017-permission-aware-identity-binding.md): its A2A route uses gateway authentication and external authorization to project bounded identity context before kagent.

For an ordinary Agent, the bridge→agent hop gives telemetry but not authz, so hitting kagent's A2A endpoint (`http://kagent-controller.kagent:8083/api/a2a/<ns>/<name>`) directly is **functionally equivalent** for fire-and-forget. Routing A2A through the gateway remains an explicit **observability/governance choice** (valuable for the showcase), **toggleable in the bridge config**. Who-may-invoke-which-ordinary-agent authorization lives in the **bridge allowlist** ([ADR 0005](0005-matrix-a2a-bridge-appservice.md)), not in the gateway. A retrieval-capable Agent is different: direct kagent routing must fail closed because it would bypass ADR 0017's identity projection.

## Consequences

1. The model credential lives in exactly one place; no agent Deployment ever holds it — the central, decisive win.
1. The ordinary A2A-through-gateway hop is honest about scope: unified telemetry and agent-card URL rewriting, not a security boundary — so the bridge allowlist remains the invocation-authorization control. ADR 0017 makes the gateway path load-bearing only for the separately admitted permission-aware retrieval capability.
1. The ordinary A2A route is a toggle: a cost-constrained or debugging deployment can point the bridge straight at `kagent-controller:8083` with no loss of correctness, only of centralised telemetry. A retrieval-capable Agent cannot use that toggle and remains unavailable when its projected gateway path is unavailable.
1. Ordinary local agent traffic stays on ClusterIP + NetworkPolicy and never touches Traefik or the public internet. The opt-in federation profile's exact public A2A listener is the separately authenticated exception documented in §8.3; kagent itself remains private.
