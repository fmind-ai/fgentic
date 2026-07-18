# Fgentic: sovereign agents in the chat room

Status: draft for human revision and publication. Do not publish without completing the [launch checklist](announcement-checklist.md), attaching a demo captured from the exact launch revision, and confirming every current-state claim.

AI agents are moving into the places where teams already work. That is useful, but the default architecture comes with a hidden decision: the chat tenant, agent identity, model route, and collaboration history usually belong to the same vendor boundary.

I wanted to test a different premise. What if the collaboration layer belonged to the organizations using it? What if agents could participate in a room without making that room, the model, or the agent runtime part of one proprietary platform?

That experiment is [Fgentic](https://github.com/fmind-ai/fgentic): an Apache-2.0, Kubernetes-native platform where humans and AI agents share self-hosted Matrix rooms. A person mentions an agent, a small Go application service delegates the task over A2A, and the answer returns as that agent's Matrix identity.

The idea is simple. The trust boundaries are not.

## The sovereignty problem is larger than model hosting

Choosing a self-hosted model does not make an agent system sovereign if identity, conversation history, policy, or inter-agent routing still depends on a closed tenant. Conversely, running every component yourself is not useful if the components only interoperate through project-specific adapters.

Fgentic treats sovereignty as a set of replaceable boundaries:

- **Matrix** is the collaboration fabric: rooms, users, events, and organization-to-organization federation.
- **A2A** is the delegation contract between the bridge and an agent, including explicitly pinned remote agents.
- **MCP** is the governed tool boundary for agents that need approved capabilities.
- **agentgateway** is the data-plane chokepoint for A2A, MCP, and model traffic.
- **kagent** is the reference Kubernetes-native agent runtime, not a permanent dependency of the bridge.
- **Fgentic's Go bridge** maps an exact Matrix agent identity to one local or remote A2A target and returns the result to the room.

These layers have independent upstream stewards. Fgentic composes them; no foundation membership, certification, or endorsement transfers to the composition. The repository's [open-stack governance map](https://github.com/fmind-ai/fgentic/blob/main/docs/open-stack.md) records those boundaries and the limits of each claim.

## The 30-second version

Open the seeded Element room. Alice types `!agents` and sees only the agents she is allowed to invoke. She then uses `!ask agent-docs-qa <question>` or mentions the agent directly.

The request follows one visible path:

```text
Matrix room
  -> Synapse application-service transaction
  -> Fgentic Go bridge
  -> agentgateway A2A route
  -> kagent Agent
  -> agentgateway model route
  -> Matrix agent reply
```

Before dispatch, the bridge resolves an exact allowlisted target, checks sender policy, applies per-sender and per-room rate limits, and admits work against bounded concurrency. The agent does not receive a model credential. The reply comes back as an `m.notice`, so other automation should not treat it as a human command.

That is the launch demo: one request, one traceable path, and the sender-filtered agent list visible before the reply. The default laptop profile uses a deterministic in-cluster response fixture. It proves the transport and policy wiring without a model credential, prompt egress, or token charge; it does **not** prove model quality. A self-hosted vLLM profile or an explicitly configured provider demonstrates a real model through the same collaboration path.

## Federation is two planes, not one magic tunnel

The long-term differentiator is cross-organization collaboration without anchoring every participant in one vendor tenant.

Fgentic separates that into two protocols:

1. Matrix federation carries the shared room and organization-level identity. Each participating homeserver signs its events and keeps a copy of the room history allowed by policy.
1. A2A carries direct machine delegation between organizations. A remote agent is admitted only through an exact route, authenticated caller, bounded token reservation, and a currently verified Signed AgentCard.

The provider-free federation lab exercises both planes. Organizations A and B exchange Matrix messages in a room-v12 room while organization C is denied. Separately, B obtains a short-lived machine JWT and invokes A's one exported docs-qa route. The lab rejects the wrong client, audience, method, path, and exhausted reservation, and returns a task-bound seller-signed receipt for the admitted request.

Those proofs are deliberately separate. The federation profile has no Matrix application service, so it does not claim a cross-organization Matrix mention-to-A2A reply. A token reservation is an admission ceiling, not observed consumption; the receipt keeps `tokensConsumed` null until per-consumer actuals exist.

## What works today

Fgentic is public and runnable on current `main`. The launch checklist binds these capabilities to one exact launch revision or a new release, so the article never attributes post-release work to an older tag:

- a Matrix mention produces a real model-backed reply on the local reference profile;
- the credential-free evaluation profile proves the complete Matrix-to-bridge-to-agentgateway-to-kagent transport;
- the bridge integration gate replaces kagent with a standalone `a2a-go` server, proving that the bridge depends on the A2A boundary rather than the kagent implementation;
- exact local and remote mappings, sender allowlists, rate limits, bounded concurrency, durable admission, and sanitized failures are tested offline;
- the federation lab proves closed Matrix federation policy and one tightly scoped inbound A2A route with positive and negative cases;
- Flux owns the GitOps composition, and production secrets are SOPS-encrypted.

The fastest evaluation path is documented in the [15-minute quick start](https://github.com/fmind-ai/fgentic#evaluate-in-15-minutes). It requires Docker, Git, mise, about 8 GiB of Docker memory, four CPUs, and 10 GiB of free disk:

```bash
git clone https://github.com/fmind-ai/fgentic.git
cd fgentic
mise install
mise run demo:up
```

The command creates only the repository-owned `fgentic-demo` cluster and prints the Element URL plus generated local credentials. `mise run demo:down` removes that evaluation cluster.

## What does not work yet

This is early, experimental, pre-1.0 software. APIs, manifests, and documentation still move.

- No production remote or public agent is enabled by default.
- The tracked GKE reference is prepared but its live apply remains spend-gated.
- The laptop k3d profiles render NetworkPolicy intent but deliberately do not enforce it. Isolation must be proved with the dedicated Calico test and on the target cluster.
- Same-organization agent rooms are intentionally unencrypted today. The federated-room policy permits only tightly scoped plaintext rooms under explicit conditions; an E2EE requirement blocks deployment until the crypto escape hatch exists.
- Prompt injection remains an unsolved trust boundary. Sender policy, scoped tools, rate limits, and model routing reduce exposure; they do not make room content trusted.
- The deterministic demo says nothing about answer quality, and the current federation lab does not prove a single end-to-end cross-org chat delegation.

These are not footnotes to hide after launch. They determine whether the system is appropriate for a given evaluation.

## Where it goes next

The [Definitive v1 focus board](https://github.com/fmind-ai/fgentic/issues/316) keeps the immediate work narrow: finish the federation wedge, the production reference, the sovereignty kit, core enterprise operations, adoption material, in-room governance controls, and agent-quality evidence. The current [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones) are the executable roadmap; later ideas remain visible but wait behind v1.

I am looking for precise feedback from three groups:

- platform teams willing to evaluate the 15-minute profile and report where the install contract breaks;
- Matrix, A2A, kagent, agentgateway, and MCP practitioners who can challenge the protocol boundaries;
- organizations exploring a real cross-organization pilot with an explicit identity, data, encryption, and cost policy.

If that is you, start with the [repository](https://github.com/fmind-ai/fgentic), the [architecture](https://github.com/fmind-ai/fgentic/blob/main/docs/architecture.md), and the [security model](https://github.com/fmind-ai/fgentic/blob/main/docs/security.md). Open a public issue for product or interoperability feedback. Report vulnerabilities privately through [SECURITY.md](https://github.com/fmind-ai/fgentic/blob/main/SECURITY.md).

One sovereign organization is useful. Two organizations collaborating across an open, governed boundary is the actual test.
