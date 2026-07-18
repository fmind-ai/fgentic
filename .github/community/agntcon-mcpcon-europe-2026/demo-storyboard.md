# Sovereign agents in the chat room — demo storyboard

Target format: 25-minute session, with 22 minutes of material and 3 minutes for questions.

Status: offline storyboard only. Live runtime operation, recordings, speaker identity, and external submission are human-owned.

## Demonstration contract

The talk must distinguish what is live, what was pre-recorded, and what is static architecture. Never describe a prior acceptance log as a live request.

The repo-owned demo and federation profiles both bind fixed local resources and are not safe parallel tenants on one host. Use one of these setups:

1. **Recommended:** present the Matrix mention-to-reply path live from one explicitly leased host and show an exact-revision, timestamped capture of the federation acceptance proof.
1. **Dual-host option:** pre-warm the demo profile on one host and the federation lab on a second isolated host, with one runtime owner per host. Rehearse display switching and both failure fallbacks.

Do not switch clusters during the 25-minute session. Do not expose generated passwords, tokens, kubeconfigs, provider credentials, unapproved room contents, shell history, or unrelated browser tabs.

## Before the event

1. Select and record the exact git SHA, model profile, tool versions, host resources, and whether each segment is live or captured.
1. Rehearse the canonical profile commands on the designated runtime host. RED documentation agents do not run `demo:*`, `fed:*`, `dev:*`, or `cluster:*`; a runtime owner must produce the evidence.
1. For the live chat segment, bootstrap the selected demo profile, log into the printed Element URL as the generated Alice user, and open the seeded lobby. Use Element-safe `!` commands.
1. Use a real, explicitly approved model profile only if the talk promises an answer or MCP tool call. The default deterministic fixture proves transport and returns a fixed sentence; it does not prove model reasoning or tool use.
1. If demonstrating MCP, rehearse a bounded, read-only `platform-helper` question and verify the expected tool stays inside the allowlist. Do not improvise a write operation or claim the deterministic fixture invoked a tool.
1. On the federation host, complete the canonical provider-free acceptance proof before the session. Retain its exact revision, timestamp, positive A/B evidence, denied-C evidence, direct A2A result, reservation behavior, and signed-receipt verification without retaining prompt or credential content.
1. Prepare a 16:9 fallback capture for each runtime segment. Label every capture with the SHA, profile, and UTC time.
1. Recheck the [speaker guide](https://events.linuxfoundation.org/agntcon-mcpcon-europe/program/speaker-guide/) if an invitation ever exists. No deck is required for this missed CFP; if a future venue accepts the talk, build the deck in Slidev using the Fmind visual system and reuse repository-native Mermaid diagrams where possible.

## 0:00–2:00 — Cold open: the missing open room

**Visual:** one Matrix room with people and agent identities; closed tenant chat logos stay outside the critical path.

**Narrative:**

> Kubernetes can host the agents, A2A can delegate to them, and MCP can expose tools. But if the human collaboration surface is still a closed tenant, the end-to-end system is not sovereign. The question is not “which chat bot?” It is “which protocol owns the room, identity, and cross-organization boundary?”

State that Fgentic is an experimental reference implementation, not the subject attendees must adopt.

## 2:00–5:00 — Separate the planes

**Visual:** a five-boundary diagram derived from the [Open Agentic Stack](https://github.com/fmind-ai/fgentic/blob/main/docs/open-stack.md).

1. Matrix: human collaboration and organization-scoped identities.
1. A2A: task delegation to an Agent.
1. MCP: scoped tool access from an authenticated workload.
1. agentgateway: A2A/model/tool routing, admission, and aggregate telemetry.
1. kagent: the reference Agent CRD, session, task, and tool runtime; replaceable at the A2A boundary.

Say which project/foundation stewards each boundary. Shared foundation hosting is not compatibility evidence.

## 5:00–9:00 — Live Matrix mention to kagent reply

**In Element:** run `!agents`, then delegate with a full ghost mention or:

```text
!ask agent-docs-qa How does this Matrix request reach the kagent Agent?
```

Point out the sender-filtered directory, working notice, threaded reply, and agent ghost identity. Walk the audience through Synapse -> Go appservice -> agentgateway -> kagent -> model -> reply.

If the profile is the deterministic fixture, read its fixed response and say immediately:

> This proves the protocol path with no credential, prompt egress, or token charge. It does not prove answer quality.

If a real approved model was rehearsed, optionally use the read-only `platform-helper` to show one allowlisted MCP query. Do not add the MCP beat when tool invocation is not visible in the retained evidence.

## 9:00–13:00 — Show the claims that stop

**Visual:** four paired statements, with “proves” on the left and “does not prove” on the right.

| Evidence                    | Proves                                                                                 | Does not prove                                                 |
| --------------------------- | -------------------------------------------------------------------------------------- | -------------------------------------------------------------- |
| Matrix event + bridge audit | An authenticated Matrix session submitted the event; the bridge asserted the full MXID | The human's real-world identity or downstream reauthentication |
| Signed AgentCard            | The fetched card matches the pinned agent signing identity                             | Caller authorization or safe transport by itself               |
| kagent task metadata        | Exact local task identity and runtime-reported task tokens                             | Exact currency cost or a unique gateway log under concurrency  |
| Token reservation           | An admission ceiling was accepted for the machine client                               | Observed token consumption, price, or payment                  |

State the NetworkPolicy caveat: repo-owned k3d renders policy intent but disables enforcement. The dedicated Calico test and target-cluster conformance provide isolation evidence; the laptop demo does not.

## 13:00–18:00 — Cross the organization boundary

**Visual:** organization A and B as separate Matrix homeservers, with denied server C outside; draw the direct A2A route separately from the shared room.

**Live or explicitly labelled capture:** show the canonical federation acceptance result from the exact candidate revision:

1. A and B exchange Matrix events in a room-v12, participant-allowlisted room.
1. C cannot join or submit the signed federation transaction.
1. A drops B's disallowed callback probe with content-free evidence.
1. B obtains a short-lived machine token and reaches only A's public docs-qa A2A route.
1. The authorized request reserves a bounded `maxTokens`; exhaustion denies later admission.
1. The terminal task returns one seller-signed, task-bound usage receipt; `tokensConsumed` remains null rather than relabeling the reservation.

Say explicitly that this lab has no Matrix appservice. It proves the collaboration federation plane and direct cross-organization A2A plane as separate contracts; it does not turn a remote Matrix mention into the public A2A request.

## 18:00–21:00 — The reusable acceptance pattern

**Visual:** a small test matrix.

1. Positive path: exact intended identity, route, task, and reply.
1. Neighbor denial: wrong sender, server, route, method, card, token, or budget.
1. Evidence boundary: content-free identifiers and signatures without prompt leakage.
1. Recovery: durable admission, bounded polling, deterministic projection, and no unsafe replay after ambiguity.
1. Replacement: rerun the boundary contract against a standalone A2A runtime, not just the reference kagent deployment.

## 21:00–22:00 — Close

**Visual:** three takeaways only.

1. Open protocols are useful when their ownership boundaries remain separate.
1. Identity, authorization, admission, and consumption are different claims.
1. A sovereign architecture is credible only when its negative paths are executable.

End with the repository evidence links, not an adoption or sales call.

## 22:00–25:00 — Questions

Keep answers inside the demonstrated evidence. Likely questions:

- **Why Matrix instead of Slack or Discord?** Matrix supplies a self-hostable room protocol and organization-to-organization federation, at the cost of more operational weight.
- **Are the rooms encrypted?** Ordinary agent rooms are plaintext by policy. The v1 partner exception is private, invite-only, joined-history, visibly plaintext, classification-bounded, and bilateral; an E2EE requirement blocks deployment.
- **Does the federation lab prove production scale?** No. It is a provider-free functional and security acceptance rig, not a capacity result or production SLA.
- **Can the model or Agent runtime be replaced?** The boundaries are designed for replacement, and the bridge integration gate proves one standalone A2A runtime. Every other replacement remains unproven until it passes the same positive and negative contract.

## Failure language

If a live segment fails, stop after one bounded retry and switch to the labelled capture:

> The live path failed at this boundary. I am switching to an exact-revision capture and will not present it as a live result. The failure itself is not evidence that the downstream control passed.

Record the failure after the session. Never weaken a check, bypass policy, expose a credential, or improvise a privileged command to rescue the stage demo.
