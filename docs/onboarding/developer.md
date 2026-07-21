---
type: Guide
title: Developer Onboarding
description: Declare, govern, map, test, and use an Agent through the Fgentic Matrix-to-A2A boundary.
---

# Developer onboarding

## 1. Delegation contract

A human `@mention` in a Matrix room becomes a non-streaming A2A `SendMessage` through the bridge. The bridge maps one local ghost to one allowlisted local kagent Agent or one explicitly pinned remote URL, preserves the full Matrix sender as asserted attribution, and posts the result in-thread as `m.notice`.

Ordinary conversations use a distinct A2A `contextId` for each `(room, ghost)` pair, so a follow-up to one Agent can continue its kagent session without sharing context with another Agent. The future permission-aware retrieval path is stricter: each initial delegation starts fresh and uses a separately typed identity-and-audience projection; do not copy ordinary context behavior into that boundary.

Read [Bridge §6](../bridge.md#6-async-delegation-as-implemented), [D5](../design-decisions.md#d5--context-threads-keyed-by-room-agent-was-per-room), and [D18](../design-decisions.md#d18--permission-aware-retrieval-binds-the-projected-identity-and-output-audience) before changing message or session semantics.

For the complete scaffold → local golden test → review → promotion → rollback workflow, follow the repository-owned [Agent authoring runbook](../../.agents/skills/matrix-agents/SKILL.md#runbook-author-and-ship-an-agent). It is the operational source for exact tasks and generated files; this onboarding page explains the security boundary those steps preserve.

## 2. Add one governed Agent

1. Run `mise run agent:new <name>` to create the Agent CRD, bridge mapping, and `evals/<name>/golden.json` fixture as one deny-by-default composition. Edit those generated files rather than introducing a parallel runtime or evaluation service.
1. Give it the smallest prompt data, service account, tool inventory, and network path it needs. Treat room content and tool results as untrusted. A read-only description in an AgentCard or MCP annotation is not an authorization decision.
1. For MCP, pin the reviewed server/tool surface and route calls through agentgateway's authenticated, audited boundary. Do not add direct Internet/tool egress to avoid the gateway contract.
1. Keep `agent-<name>` mapped to exactly one `namespace`/`name`, retain `stage: dev`, and authorize only explicit full MXIDs in `allowedSenders`; never authorize by display name or localpart.
1. If an approved optional network bridge must relay this ghost, add the exact ghost permission only to that network's opt-in unit. Do not put provider identities or permissions into the canonical cluster-independent map.
1. Update the welcome/gallery text when the new Agent should be discoverable. Cached AgentCard metadata is descriptive and quarantined when remote trust fails.

## 3. Prove the boundary

Run focused deterministic checks first:

```bash
mise run agent:test <name>
mise run check:agents
mise --cd apps/matrix-a2a-bridge run test
```

`agent:test` runs only `evals/<name>/golden.json` against the same deterministic loopback model and assertion code used by CI's all-Agent gate. It starts no cluster, makes no external request, and spends no model tokens.

Then let the repository hooks and CI run the complete gates. Tests should cover at least:

1. exact local ghost and homeserver resolution, allowed/denied full senders, and a foreign look-alike ghost;
1. Agent schema, model reference, service account, tools, NetworkPolicy, and immutable pins;
1. successful A2A reply plus sanitized denial/failure behavior without content leakage;
1. distinct context per room/Agent and the required fresh-session exception where applicable;
1. rate/capacity limits, loop-safe `m.notice` output, deduplication, and attribution identifiers;
1. negative MCP/workload authentication and an unapproved tool call;
1. federation-safe behavior if any remote sender or Agent is in scope.

Runtime acceptance uses the smallest owned environment that exercises the changed boundary and only one mutating runtime owner. A unit test is not target-cluster evidence; a successful live reply is not permission, load, recovery, or security evidence.

## 4. Use and debug

Invite the mapped ghost, run `!agents` and `!agents <name>` to inspect the sender-filtered cached entry, then send one explicit mention. Follow the event ID through bridge audit/metrics, A2A task identity, kagent, agentgateway token metrics, and the Matrix reply using [Delegation Attribution Audit](../audit.md). Prometheus model metrics are aggregate and do not authenticate a user.

Do not log prompts, replies, credentials, raw files, or tool payloads to make debugging easier. Keep errors classified and content-free at the bridge/audit boundary, and reproduce sensitive failures with synthetic input.

> **Own vs compose.** Fgentic owns the Matrix-to-A2A bridge, ghost allowlist, governance wiring, and reference Agent composition. kagent owns Agent execution and sessions; agentgateway owns proxy/routing behavior; MCP servers own tool semantics; the selected backend owns inference. Extend the boundary through their supported APIs rather than duplicating those systems in the bridge.
