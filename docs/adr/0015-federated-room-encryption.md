---
type: Architecture Decision Record
title: Federated Agent Rooms Remain Unencrypted for v1
description: Keep v1 federated agent rooms unencrypted only within a tightly scoped, classified, contract-bound collaboration surface.
---

# 0015 — Federated Agent Rooms Remain Unencrypted for v1

Status: Accepted

Approval: [maintainer decision for issue #56](https://github.com/fmind-ai/fgentic/issues/56#issuecomment-4965773991)

## Context

[ADR 0008](0008-unencrypted-agent-rooms.md) keeps same-organization agent rooms unencrypted so the Matrix appservice can remain crypto-free. That deployment rationale does not survive federation: an operator of any participating homeserver can read and retain every unencrypted event delivered to it, even after redaction or offboarding. Access to events sent before a user joins depends on the room's history-visibility state, so that state is part of the decision rather than an assumed default.

[Matrix room E2EE](https://spec.matrix.org/latest/client-server-api/#end-to-end-encryption) already uses Olm/Megolm. Supporting it here would make the bridge a crypto client responsible for device identity, keys, cross-signing trust, recovery, durable crypto state, and restart/HA behavior. mautrix supports end-to-bridge encryption, but its sync-less appservice mode still depends on experimental [MSC3202](https://github.com/matrix-org/matrix-spec-proposals/pull/3202) transaction extensions and is [explicitly not recommended](https://docs.mau.fi/bridges/general/end-to-bridge-encryption.html); MAS deployments add the [MSC4190](https://github.com/matrix-org/matrix-spec-proposals/pull/4190) device-management path. Fgentic does not currently implement or operate that boundary.

The v1 choices are therefore:

1. Permit only purpose-built, low-sensitivity federated agent rooms, with technical scoping and bilateral contractual controls.
1. Implement and operate appservice E2EE before allowing any federated agent room.
1. Forbid federated agent rooms and lose the platform's defining cross-organization collaboration path.

The first option preserves the v1 wedge without pretending that transport controls provide content confidentiality.

## Decision

For v1, production or real-partner federated agent rooms remain **unencrypted** and are allowed only under all of the following controls. The synthetic acceptance-rig exception is defined narrowly below.

1. Create a new room for one named project or bounded business purpose. Do not federate an existing room or its history. Name the owners, approved partner servers, users, agents, review date, and offboarding owner before activation.
1. Keep every unrelated or sensitive room local by creating it with immutable `m.federate: false`. Human-only rooms may use Matrix E2EE, but the crypto-free bridge and its ghosts must not join them.
1. Create the federated room as private and install [`m.room.join_rules: invite`](https://spec.matrix.org/latest/client-server-api/#mroomjoin_rules) plus [`m.room.history_visibility: joined`](https://spec.matrix.org/latest/client-server-api/#mroomhistory_visibility) in its initial state. Invite only the named users and agents. A [partner server ACL](https://spec.matrix.org/latest/client-server-api/#server-access-control-lists-acls-for-rooms) is a network boundary, not user-membership authorization.
1. Apply the full [§8.2 border](../federation.md#82-required-hardening-all-git-declared) at room creation: room version 12 or newer, closed homeserver allowlists, an initial partner-only `m.room.server_acl`, the fail-closed callback policy, and deny-by-default bridge sender policy ([D6](../design-decisions.md)).
1. Put the room's purpose, plaintext/replication warning, allowed data class, and owner in its operating record and visible room guidance. A user must not infer confidentiality from an invite-only room or TLS.
1. Apply this data-classification policy to text, attachments, prompts, replies, and agent artifacts:

| Data class                  | May be pasted into a v1 federated agent room? | Required handling                                                                                                                                           |
| --------------------------- | --------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Public                      | Yes                                           | Confirm the content is appropriate for the room purpose and any invoked model or tool.                                                                      |
| Partner-approved non-public | Only when explicitly named in the room record | Share the minimum necessary, redact unrelated identifiers, and ensure the bilateral agreement covers every homeserver, model, tool, backup, and operator.   |
| Restricted or regulated     | No                                            | Use a redacted or synthetic excerpt, or an access-controlled reference handled outside the room. A new approved design is required before this class moves. |
| Secrets or authentication   | Never                                         | Do not share credentials, tokens, private keys, recovery material, or unredacted security evidence. Treat exposure as an incident and rotate immediately.   |

1. Complete the bilateral [federation onboarding agreement](../federation-onboarding.md) before activating a production or real-partner room. It must cover purpose and roles, permitted and prohibited data, residency, retention and backups, best-effort redaction, subprocessors and model providers, data-subject requests, incident response, review, and offboarding. A blank or disputed field blocks the room.
1. Keep audit and policy-denial evidence content-free. Agents and tools receive only the minimum content required for the approved purpose; their provider and retention boundaries remain separate approvals.

The provider-free `fgentic-fed` acceptance rig may omit a bilateral agreement and use test-specific join/history state only while every organization, identity, credential, and event is synthetic and every fixture is public or generated. It retains the §8.2 technical border, runs on a disposable cluster lifecycle, and must never carry a real partner or non-public data. The rig's code remains permanent acceptance infrastructure under [ADR 0013](0013-federation-lab-acceptance-rig.md); this narrow data-handling exception is not a production deployment precedent.

This stance is revisited through a new ADR when any of these triggers fires:

1. A partner, regulator, or adopter requires confidentiality from participating homeserver operators.
1. A real use case requires restricted, regulated, or otherwise prohibited data in the shared room.
1. mautrix appservice E2EE has a supported, non-experimental Synapse/MAS path and Fgentic can prove device provisioning, cross-signing, key backup/recovery, restart, and HA behavior across two homeservers.
1. Matrix MLS or Decentralized MLS reaches an accepted specification with compatible server, client, and Go SDK implementations. MLS is research, not a v1 dependency; [MSC4143 is MatrixRTC](https://github.com/matrix-org/matrix-spec-proposals/pull/4143), not a room-encryption proposal.
1. An incident or control review shows that classification and contractual controls are insufficient.

Until a replacement is implemented and accepted, an E2EE requirement means the federated agent room is not deployed; it is not waived.

## Consequences

1. Fgentic can ship its cross-organization collaboration path without adding an immature crypto operating surface to the small appservice bridge.
1. Every admitted homeserver operator can read the room content, and already replicated history cannot be reliably retracted. The allowed data class and bilateral agreement are therefore security controls, not documentation niceties.
1. Closed federation, ACLs, callback policy, TLS, and sender authorization reduce who can participate and invoke agents, but none of them encrypts content from an admitted partner.
1. The stance deliberately excludes restricted, regulated, and secret material from v1 federated agent rooms. Deployments needing those classes must keep the workflow local or fund the E2EE escape hatch.
1. The future change is bounded but substantial: add bridge crypto state and operations, remove the agent-room force-disable for the approved scope, and prove cross-homeserver key lifecycle and failure recovery before changing the classification policy.
