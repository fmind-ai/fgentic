# AGNTCon + MCPCon Europe 2026 proposal

Status: prepared after the deadline; **not submitted and no longer eligible through the public CFP**.

Verified on 2026-07-18 against the official [Linux Foundation CFP](https://events.linuxfoundation.org/agntcon-mcpcon-europe/program/cfp/) and [Sessionize submission page](https://sessionize.com/agntcon-mcpcon-europe-2026/).

## Verified venue and format

| Field            | Verified state                                                                                                                                                                                                                                                                              |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Event            | AGNTCon + MCPCon Europe, 17–18 September 2026, RAI Amsterdam                                                                                                                                                                                                                                |
| Public CFP       | Closed 8 June 2026 at 23:59 CEST (UTC+02:00); Sessionize no longer accepts submissions                                                                                                                                                                                                      |
| Session types    | Session presentation: 25 minutes; panel: 25 minutes; workshop: 60 minutes                                                                                                                                                                                                                   |
| Proposed format  | 25-minute session presentation                                                                                                                                                                                                                                                              |
| Primary topic    | Interoperability, Protocols (MCP, A2A, etc.) and Standards                                                                                                                                                                                                                                  |
| Secondary topics | Human-Agent Collaboration; Building Reliable Agent Systems; Enterprise Adoption in Practice                                                                                                                                                                                                 |
| CFP constraints  | No product or vendor sales pitch; avoid unlicensed or potentially closed-source technology; make the speaker's subject-matter expertise and specific experience assessable. The closed public pages expose no current abstract length limit, so this draft does not invent one.             |
| Human gate       | Do not attempt a late submission unless the organizers explicitly reopen the CFP or invite one. Any submission must be reviewed, personalized, and filed by the speaker under their own identity; the current Europe 2026 acceptance criterion can no longer be met through the public CFP. |

## Proposed session

### Title

Sovereign agents in the chat room: an end-to-end open stack (Matrix + A2A + MCP + agentgateway)

### Abstract

Agent runtimes are moving into Kubernetes, but the humans delegating work to them often remain inside a closed chat tenant or a bespoke single-user UI. What changes when the collaboration plane is an open, federated protocol too?

This session follows one implemented delegation from a Matrix room through a small Go application-service bridge, A2A, agentgateway, and a Kubernetes-native kagent Agent, then back to the room as a threaded agent reply. A read-only MCP tool route shows why tool identity and allowlists belong at a separate boundary. A provider-free federation lab then separates two cross-organization concerns that are often collapsed: Matrix federation for shared-room collaboration, and authenticated A2A for direct machine delegation.

The useful lessons are in the negative cases. A Matrix sender assertion is attribution, not downstream authentication. A signed AgentCard proves an agent identity, not caller authorization. A token reservation limits admission; it is not observed consumption or a bill. Plaintext federated rooms require explicit classification and bilateral policy.

Attendees leave with a standards-based boundary map, concrete fail-closed controls, and an acceptance-test pattern they can apply without adopting Fgentic or any single runtime.

### Audience

Platform architects, agent-framework and protocol implementers, security engineers, and enterprise teams evaluating human-agent collaboration across organizational boundaries. The material assumes basic familiarity with Kubernetes and agent protocols but not with Matrix.

### Learning outcomes

Attendees will be able to:

1. separate collaboration, delegation, tool, model-egress, and identity responsibilities across Matrix, A2A, MCP, agentgateway, and a replaceable Agent runtime;
1. identify where sender attribution, AgentCard identity, authorization, token reservation, and measured consumption stop proving the next claim; and
1. design provider-free positive and negative acceptance evidence for a cross-organization agent path.

### Why this is not a product pitch

The talk uses Fgentic as one Apache-2.0 reference implementation and names its experimental pre-1.0 status. Each protocol/component boundary has independent stewardship, and the conclusion is a reusable control and test pattern rather than an adoption request. The bridge integration suite also replaces kagent with a standalone `a2a-go` server to test runtime swappability instead of merely claiming it.

### Evidence behind the proposal

- [Open Agentic Stack governance map](https://github.com/fmind-ai/fgentic/blob/main/docs/open-stack.md)
- [Bridge behavior and A2A boundary](https://github.com/fmind-ai/fgentic/blob/main/docs/bridge.md)
- [Delegation attribution evidence and limits](https://github.com/fmind-ai/fgentic/blob/main/docs/audit.md)
- [Security model](https://github.com/fmind-ai/fgentic/blob/main/docs/security.md)
- [Federation design and provider-free lab](https://github.com/fmind-ai/fgentic/blob/main/docs/federation.md)
- [Model and evaluation profiles](https://github.com/fmind-ai/fgentic/blob/main/docs/models.md)

## Speaker-owned revision before any reuse

The CFP warns that generic or templated submissions make expertise hard to assess. Before reusing this draft, the human speaker must:

1. replace general phrasing with the exact implementation decisions, failures, and measurements they personally intend to defend;
1. confirm every version, command, and live claim against the candidate revision;
1. decide which model boundary and data classification the demonstration will use;
1. adapt the abstract to the actual venue fields and current themes rather than assuming the closed Europe 2026 form; and
1. remove any claim that the rehearsed evidence cannot still reproduce.

No organizer acceptance, speaker slot, or AAIF endorsement is implied by this repository draft.
