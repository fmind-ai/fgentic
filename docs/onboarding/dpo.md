---
type: Guide
title: DPO Onboarding
description: Data-flow, residency, retention, federation, and classification questions for a Fgentic privacy review.
---

# DPO onboarding

## 1. Privacy decision

Approve only named room purposes, data classes, participants, Agents, tools, model profiles, retention periods, federation peers, and evidence owners. “Self-hosted” describes the operated stack; it does not mean every selected model or participating homeserver keeps data inside one cluster.

No processor, region, retention term, or legal basis is assumed here. Those are deployment and contract decisions.

## 2. Data-flow map

| Data                         | Where it goes                                                                                                                                                                                  | Decision and evidence                                                                                                                                            |
| ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Matrix room events and files | Synapse stores the room; the bridge receives eligible appservice events. Agent rooms are unencrypted by policy.                                                                                | Record room purpose, membership, classification, retention, file policy, and visible plaintext warning. See [ADR 0008](../adr/0008-unencrypted-agent-rooms.md).  |
| Delegation prompt and reply  | The bridge sends the minimum delegated message over A2A; the Agent returns content that the bridge posts as a Matrix `m.notice`.                                                               | Review [Bridge §6](../bridge.md#6-async-delegation-as-implemented), Agent prompt/tool scope, media limits, and human review process.                             |
| Model request and response   | agentgateway sends them to exactly one selected profile. Self-hosted vLLM serves in-cluster after its bootstrap; API profiles send complete requests and responses to the configured provider. | Approve the [model data flow](../models.md#data-flow-and-residency), account contract, deployment geography, retention, subprocessors, and billing boundary.     |
| Identity and audit metadata  | The bridge records full Matrix identity and content-free task/event identifiers; operational metadata can still be linkable.                                                                   | Apply access, retention, and purpose limits to [audit evidence](../audit.md) and [observability metadata](../observability.md). “Content-free” is not anonymous. |
| Service state                | CloudNativePG stores separate service databases; the GCP reference declares 30-day WAL/base-backup retention intent.                                                                           | Confirm the actual storage class, object store, encryption, access, retention, restore, and deletion behavior. A backup manifest is not restore evidence.        |
| Federated room history       | Every participating homeserver receives room state and history. Cross-server redaction is best effort.                                                                                         | Use [Federation §8.1](../federation.md#81-what-matrix-federation-gives--stated-honestly) and the bilateral agreement before inviting a partner.                  |

The repository does not yet ship a complete retention/GDPR-erasure policy pack; [#137](https://github.com/fmind-ai/fgentic/issues/137) remains open. Do not convert database backup retention, Matrix redaction, or account disablement into an unsupported erasure claim.

## 3. Federated-room classification

[ADR 0015](../adr/0015-federated-room-encryption.md) permits a real-partner v1 room only when it is private, invite-only, joined-history, purpose-scoped, visibly plaintext, classification-bounded, and contract-bound:

1. Public data may be shared when the room purpose allows it.
1. Partner-approved non-public data requires an explicit room record, minimum-necessary disclosure, and confirmed partner handling.
1. Restricted or regulated data is not allowed; use a redacted/synthetic excerpt or an access-controlled reference outside the room.
1. Secrets and authentication material are never allowed.

These limits apply to text, files, prompts, replies, retrieved context, and Agent artifacts. Once a participating homeserver has replicated history, neither redaction nor offboarding guarantees erasure from that operator's systems.

## 4. Privacy approval record

1. Inventory rooms, Agents, tools, data subjects, purposes, classifications, recipients, homeservers, model paths, and storage locations.
1. Record minimization rules for prompts, retrieval, files, replies, audit evidence, and support bundles.
1. Separate application retention, database backup retention, object-store lifecycle, audit retention, and partner copies; assign deletion and legal-hold owners to each.
1. Verify identity lifecycle and room removal across MAS/IdP, Synapse, the bridge allowlist, agentgateway/federation credentials, and retained backups.
1. Review every API model provider and federation partner under the organization's own legal and transfer process.
1. Test an isolated restore and an offboarding/redaction scenario, then record what remains and why.
1. Set a review trigger for new tools, model profiles, partners, room classifications, retention changes, or an E2EE requirement.

> **Own vs compose.** Fgentic owns the bridge/governance/federation configuration and documents the data boundaries it creates. It composes storage from CloudNativePG/PostgreSQL, identity and room state from Matrix/ESS and the IdP, Agent execution from kagent, routing from agentgateway, and inference from vLLM or an API provider. Each operator remains responsible for its deployment and contracts.
