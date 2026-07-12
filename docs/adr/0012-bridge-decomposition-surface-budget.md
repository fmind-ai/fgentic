---
type: Architecture Decision Record
title: Bridge Decomposition and Surface Budget
description: Decompose the bridge under an explicit surface budget that caps what the core may grow.
---

# 0012 — Bridge Decomposition and Surface Budget

Status: Proposed

## Context

The bridge ([ADR 0005](0005-matrix-a2a-bridge-appservice.md)) is deliberately small, and its `replicas: 1` + `Recreate` posture is an enforced invariant (D3, [docs/bridge.md](../bridge.md) §5): the appservice protocol is single-consumer and per-room ordering is keyed in one process. The 2026-07-12 roadmap wave concentrates 29 open issues on this one component, proposing accretions of very different kinds: Matrix-plane rendering (threads, polls, media, profile fields, task state events), outbound drivers (extension negotiation, sandbox lifecycle, room context digests), and — critically — **new inbound network surfaces** (an MCP server for rooms, an A2A push-notification webhook receiver, a read-only admin page). Each issue hedges "or a sibling app — justify" individually; nobody owns the aggregate. Without a rule, the bridge drifts into a monolith whose every new inbound surface inherits deploy-time downtime from the single-replica invariant, and whose blast radius grows with each feature.

## Decision

1. The bridge binary's scope is the **Matrix appservice delegation plane**: mention → policy → A2A → reply, plus Matrix-plane rendering of delegation state (edits, threads, polls, pinning, media round-trip, profile fields, task state events, room context digests) and **outbound** drivers it alone can key (per-room activity → sandbox pause/resume, extension negotiation, delayed-event heartbeats). Issues #114–#120, #173, the state-publisher half of #121, and #150's driver stay in-bridge.
1. **Surface budget rule:** any new **inbound network surface** beyond the appservice transaction endpoint, the metrics port, and cluster-internal webhook callbacks — and any component with an independent availability or scaling profile — ships as a **sibling self-contained app** under `apps/` (own Go module, chart, Flux unit, NetworkPolicy), never inside the bridge binary. Concretely: the Matrix MCP server (#131), any remote-facing push receiver (#124's remote half), and a standalone operator console (#140, if built beyond read-only-in-bridge) are sibling apps.
1. A2A push notifications (#124) are **cluster-internal only** in v1, served by the bridge's existing HTTP server as a wake-up signal; exposing a remote-facing receiver requires a sibling app **and** an amendment to the "sole public A2A exception" principle in [.agents/AGENTS.md](../../.agents/AGENTS.md) and [docs/federation.md](../federation.md).
1. The single-replica, per-room-ordering invariant (D3) is not weakened by any accretion; sibling apps must tolerate the bridge's `Recreate` window without data loss (state lives in Postgres or Matrix room state, never in cross-app memory).
1. **Not done:** no message-bus or microservice split of the core delegation path, and no bridge HA work — both remain out of scope until measured need (Principle 10), revisited only by a future ADR.

## Consequences

1. The bridge stays reviewable and its threat surface bounded: inbound surfaces are enumerable per app, each behind its own NetworkPolicy and Flux unit.
1. More Flux units and charts to maintain — the accepted cost; the repo's self-contained-app convention (bridge, synapse-federation-policy) already amortizes it.
1. Issues #124, #131, and #140 are re-scoped to comply (cluster-internal v1, sibling app, and inventory-first respectively); future issues cite this ADR instead of re-arguing placement.
1. Escape hatch: if sibling apps proliferate past roughly four, or the Recreate window becomes a measured availability problem, a new ADR revisits decomposition and the single-replica invariant together.
