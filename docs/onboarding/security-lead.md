---
type: Guide
title: Security Lead Onboarding
description: Trust boundaries, enforcement points, evidence, and known limitations for a Fgentic security review.
---

# Security lead onboarding

## 1. Security decision

Approve a deployment only when the selected room, sender, Agent, tools, model path, federation peers, and operational evidence form one reviewed boundary. Self-hosting moves control to the adopter; it does not remove prompt injection, identity, supply-chain, or operations risk.

The complete trust-zone table and threat model live in [Security Boundaries](../security.md) and [`docs/security/threat-model.md`](../security/threat-model.md). The auditor-oriented control mapping remains tracked by [#374](https://github.com/fmind-ai/fgentic/issues/374); this guide does not claim that dossier exists.

## 2. Enforcement map

| Question                                     | Enforcement point                                                                                                                                                                                                           | Evidence to review                                                                                                                                                                                             |
| -------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Who can invoke an Agent?                     | The bridge resolves only local agent ghosts, then evaluates each Agent's full-MXID `allowedServers` and `allowedSenders` policy. Federated senders deny by default.                                                         | [D6](../design-decisions.md#d6--federation-safe-target-resolution--sender-policy-was-localpart-spoofing-hole), [`agents.schema.json`](../../apps/matrix-a2a-bridge/agents.schema.json), rendered `agents.yaml` |
| What authenticates the bridge workload?      | agentgateway requires the bridge's A2A workload key. The forwarded `X-User-Id` remains asserted attribution and is not a downstream end-user credential.                                                                    | [§7.2–§7.3](../security.md#72-attribution-is-not-downstream-authentication), [audit runbook](../audit.md)                                                                                                      |
| Where can model traffic go?                  | One selected agentgateway profile owns the model credential and route; NetworkPolicies constrain the selected path.                                                                                                         | [Model data flow](../models.md#data-flow-and-residency), [`infra/agentgateway/`](../../infra/agentgateway/), [`infra/models/`](../../infra/models/)                                                            |
| Which Kubernetes Agent objects are admitted? | Cluster-wide admission policies require one approved model gateway, reviewed service-account identity, and a scoped MCP subset. Other policies reject mutable image tags, lost PSS labels, and unapproved Service exposure. | [`infra/policies/`](../../infra/policies/), [§7.6](../security.md#76-admission-enforced-platform-invariants)                                                                                                   |
| Which tools can one Agent call?              | The governed MCP route authenticates one workload and authorizes the pinned tool inventory. Tool annotations remain untrusted metadata; all current tools retain their destructive/open-world flags.                        | [§7.4](../security.md#74-governed-mcp-tool-egress), [`infra/agentgateway/mcp-surface.pin.json`](../../infra/agentgateway/mcp-surface.pin.json)                                                                 |
| What bounds model spend and loops?           | Per-sender/per-Agent and per-room invocation buckets, bounded durable queues, `m.notice` replies, gateway token metrics, and the token-burn alert.                                                                          | [D7/D8](../design-decisions.md), [Observability §9](../observability.md)                                                                                                                                       |
| What crosses an organization boundary?       | Room-v12 policy, closed federation, server ACLs, a Synapse callback border, bridge sender policy, Signed AgentCards, and reviewed JWT or mTLS authorization.                                                                | [Federation §8](../federation.md), [partner onboarding](../federation-onboarding.md)                                                                                                                           |

## 3. Prompt-injection posture

Room text, files, remote AgentCards, model output, retrieved content, and MCP results are untrusted. Sender allowlists answer “who may invoke”; they do not make the prompt safe. Review the Agent's tools and service account for the least privilege needed, keep high-impact writes behind an explicit approval design, and test malicious room content against the exact deployed revision.

Do not infer a general data-loss-prevention control from model routing. The model profile and NetworkPolicies constrain a network path, while prompt/response classification enforcement remains separate roadmap work. A denial or content-free audit record also does not prove that unrelated content never reached another component.

## 4. Minimum approval evidence

1. Record the repository revision, effective cluster overlay, rendered Agents, bridge map, model profile, MCP pin, and federation policy digest.
1. Run the static policy, NetworkPolicy, admission, supply-chain, vulnerability, and secret-leak gates without skips or suppressions.
1. On the target environment, prove allowed and denied senders, wrong workload credentials, unapproved tools, unapproved services, external model egress, and the self-hosted no-egress path where selected.
1. Retain content-free delegation attribution evidence and separately protect any operational metadata that still identifies rooms, users, events, or tasks.
1. Name owners for vulnerabilities, identity, database, model, federation, incident response, backup/restore, and upstream escalation.
1. Re-run negative controls after policy, model, chart, protocol, or federation-partner changes.

Known limits must remain visible: same-organization agent rooms are plaintext; federated history cannot be reliably retracted after replication; ordinary `X-User-Id` forwarding is not authenticated downstream identity; and a green static render is not live production evidence.

> **Own vs compose.** Fgentic owns the bridge's invocation policy, governance/federation wiring, admission contracts, and reference evidence. It composes Matrix/ESS, kagent, agentgateway, CloudNativePG, model providers, Kubernetes, and their security/support processes. Escalate component defects upstream while preserving the Fgentic boundary tests.
