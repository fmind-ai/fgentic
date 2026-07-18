# Fgentic for kagent

**A federated, chat-native human interface for kagent agents.**

Fgentic lets people invoke Kubernetes-native kagent agents from Matrix rooms. A user `@mention`s an agent—or uses the Element-safe `!ask` command when the client cannot autocomplete ghost users—and the Go Matrix Application Service delegates the request over A2A, then posts the result back as that agent's Matrix identity.

Fgentic is experimental, pre-1.0 integration software. It composes kagent; it does not fork, bundle, or replace the kagent runtime.

## Why this belongs beside kagent

kagent already documents A2A adapters for [Slack](https://kagent.dev/docs/kagent/examples/slack-a2a) and [Discord](https://kagent.dev/docs/kagent/examples/discord-a2a). Fgentic explores a different collaboration boundary:

- **Rooms, not a single bot endpoint.** Humans and agent ghosts are first-class members of a shared conversation, with replies and long-task updates related to the original event.
- **Federation, not one vendor tenant.** Matrix homeservers can form governed cross-organization rooms while each organization retains its own identity and infrastructure boundary.
- **A2A-native runtime reuse.** The bridge maps a Matrix ghost to an existing kagent `Agent` by namespace and name. kagent remains the Agent CRD, session, task, and tool runtime.
- **Policy before invocation.** Exact agent mappings, sender/server allowlists, per-sender and per-room rate limits, durable admission, and bounded concurrency apply before an A2A request can spend model capacity.
- **Attribution with honest limits.** The full Matrix ID is forwarded as `X-User-Id` and persisted with local kagent session/task evidence. It is an attribution assertion from the trusted appservice path, not a second authentication at kagent. Enforced NetworkPolicy is therefore a required deployment control. The repo-owned k3d demo renders that intent but deliberately disables enforcement; only the dedicated Calico test and target-cluster conformance can prove isolation.
- **Cost controls without false precision.** agentgateway is the model-egress chokepoint and exposes aggregate token telemetry. kagent supplies per-task token metadata. Cross-organization reservations are admission ceilings, not observed consumption.

## The composed path

```text
Matrix user
  -> Synapse appservice transaction
  -> Fgentic Matrix-to-A2A bridge
  -> agentgateway A2A route
  -> kagent Agent
  -> agentgateway model route
  -> kagent task result
  -> Matrix agent reply
```

The bridge uses the official [`a2a-go`](https://github.com/a2aproject/a2a-go) SDK. Local mappings target kagent's A2A service. Optional remote mappings are separate trust boundaries and fail closed unless the exact endpoint presents a currently verified, pinned ES256/JCS Signed AgentCard.

## What is ready to show

- A credential-free `mise run demo:up` profile proves the full Matrix -> bridge -> agentgateway -> kagent transport with an in-cluster deterministic response fixture.
- A self-hosted vLLM profile or configured provider can demonstrate a real model without changing the collaboration path.
- The seeded Element room exposes `agent-docs-qa`, `agent-platform-helper`, and `agent-scribe`; `!agents`, `!ask`, and `!budget` make the governed surface visible without relying on Matrix autocomplete.
- Offline CI covers the A2A client contract, durable bridge behavior, agent configuration, deterministic Agent golden tasks, and an isolated real Matrix appservice round trip against a standalone A2A server.

The default demo response proves wiring, not model intelligence. The production-shaped profiles add controls deliberately omitted from the laptop demo, including SSO, observability, and runtime vulnerability scanning.

## Proposed community relationship

Start small: document Fgentic as a maintained Matrix/A2A integration that **works with kagent**. The concrete offer is an upstream integration guide or ecosystem entry with an owner, tested version boundary, architecture diagram, and honest security limitations.

If kagent maintainers see a reusable upstream boundary beyond documentation, discuss that boundary before proposing subproject status. The immediate request is feedback on where a maintained Matrix integration belongs and what acceptance evidence the community expects.

## Evidence

- [Fgentic architecture and evaluation path](https://github.com/fmind-ai/fgentic#readme)
- [Bridge behavior and trust boundaries](https://github.com/fmind-ai/fgentic/blob/main/docs/bridge.md)
- [Delegation attribution audit](https://github.com/fmind-ai/fgentic/blob/main/docs/audit.md)
- [Federation design and executable lab](https://github.com/fmind-ai/fgentic/blob/main/docs/federation.md)
- [Matrix-to-A2A bridge source](https://github.com/fmind-ai/fgentic/tree/main/apps/matrix-a2a-bridge)
- [Apache-2.0 license](https://github.com/fmind-ai/fgentic/blob/main/LICENSE)
