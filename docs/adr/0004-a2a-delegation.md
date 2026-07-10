# 0004 — A2A (`message/send`, Non-Streaming) via `a2a-go` v2

Status: Accepted

## Context

When a room message targets an agent, the bridge must relay it to that agent and post the reply back ([ADR 0005](0005-matrix-a2a-bridge-appservice.md)). The delegation gesture is inherently **fire-and-forget**: post a task, get a reply. The transport should be an open, governed standard that the agent runtime (kagent) already speaks, with typed, self-describing endpoints.

Alternatives considered and rejected:

1. **Hand-rolled JSON-RPC over HTTP.** Reinvents typed transport, the error taxonomy, and AgentCard discovery that a standard SDK already provides — pure technical debt.
1. **MCP-only (invoke agents as MCP tools).** MCP is the right protocol for _tools_, not for _agent-to-agent task delegation_; it lacks the AgentCard/agent-identity model and would misframe the interaction.
1. **`trpc-group/trpc-a2a-go`.** A viable A2A implementation with a lower Go floor, but not the official SDK — rejected on the "latest-stable + official" preference and DRY with kagent.
1. **A2A streaming (`message/stream`, SSE).** Adds SSE plumbing, partial-result handling, and connection lifecycle for no benefit to a single-round-trip delegation.

## Decision

Invoke agents over **A2A** (spec v0.3.0, proto "1.0"; a Linux-Foundation-governed standard) using the official Go SDK **`github.com/a2aproject/a2a-go/v2`** (`a2aclient`), **`message/send` only** — a synchronous round trip returning a `Task | Message` sum type. **Streaming is deliberately unused.** Agents are discovered and validated via their **AgentCard** at `/.well-known/agent-card.json`. This is the **same SDK kagent uses**, so the boundary is type-safe and DRY.

## Consequences

1. The interaction maps exactly onto the `@mention` gesture — one request, one reply, no streaming state to manage.
1. `SendMessageResult` is a sum type; extracting text from a completed `Task` (Status.Message / last artifact) versus a bare `Message` is fiddly — isolated in one well-tested `SendMessageResult → string` helper, unit-tested against a real `a2asrv` fixture.
1. AgentCard discovery makes agents self-describing; unknown or invalid targets are rejected fast, before any LLM cost is incurred.
1. A synchronous `message/send` blocks on slow agents — bounded by a 60s context deadline plus an optional "working…" notice; `tasks/get` polling is adopted only if genuinely long-running agents appear.
1. Sharing kagent's SDK means A2A version bumps are coordinated across both — a deliberate, low-cost coupling.
