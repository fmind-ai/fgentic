---
type: Reference
title: Incumbent Chat Coexistence
description: Choose whether to run Fgentic beside, bridge with, or migrate from an incumbent self-hosted chat system.
---

# Incumbent chat coexistence

Fgentic does not require an organization to replace Mattermost, Rocket.Chat, Zulip, or Microsoft Teams before it can evaluate sovereign agents. The default posture is to keep the incumbent as the human chat plane and use purpose-scoped Matrix rooms as the agent collaboration plane. Bridging and migration are separate, progressively larger decisions.

This guide distinguishes three products that are easy to conflate:

1. An **agent adapter** lets a user invoke selected Fgentic A2A agents from the incumbent chat. It does not mirror a room into Matrix.
1. A **room bridge** copies conversations between the incumbent and Matrix. A production bridge must reconcile identity, membership, threads, edits, deletion, files, retention, and retries—not merely forward text through a bot.
1. A **notification relay** sends labeled, usually one-way status. It is neither an identity bridge nor an interactive agent plane.

## Choose a posture

| Question        | Beside: agent rooms only                                              | Bridge: selected rooms                                                          | Migrate: Matrix becomes human and agent chat                                                  |
| --------------- | --------------------------------------------------------------------- | ------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| Best fit        | Fast evaluation; incumbent remains the system of record               | A small set of workflows cannot tolerate a second client                        | The organization has independently decided to replace its incumbent chat                      |
| User experience | Users open Element only for agent work                                | Users may participate from either client, within the bridge's semantic limits   | Users move daily chat to a Matrix client                                                      |
| Data movement   | No incumbent messages move                                            | New selected-room content is copied into both systems                           | Accounts, active groups, selected history or an archive, and operating procedures move        |
| Identity        | Separate accounts; the same IdP can reduce login friction             | Separate accounts plus an explicit bridge identity map                          | New Matrix IDs become primary collaboration identities                                        |
| Federation      | Only Matrix agent-room content may federate                           | Bridged content may be replicated to every participating Matrix homeserver      | New Matrix rooms may federate under the organization's federation policy                      |
| Main loss       | Context must be pasted or summarized deliberately; two clients remain | Some identity and event semantics are inevitably lossy; two systems retain data | Migration effort, user retraining, integration replacement, and incomplete history conversion |
| Reversal        | Remove the Fgentic rooms and accounts                                 | Stop the bridge, revoke credentials, then reconcile the two divergent histories | Keep the incumbent read-only archive or execute its separately tested rollback plan           |
| Recommendation  | **Default**                                                           | Only after a bridge-specific qualification                                      | Only as an independently approved chat migration program                                      |

Choose **beside** unless a named workflow proves that opening a purpose-scoped agent room is unacceptable. Choose **bridge** only when the exact incumbent/version/room types pass the qualification in this guide. Choose **migrate** only when replacing the human chat system is already an organizational goal; Fgentic is not sufficient justification by itself.

## Posture 1: run beside the incumbent

Keep human conversations in the incumbent and create narrowly scoped Matrix rooms for work that benefits from governed agents or cross-organization collaboration. Users carry only the minimum required context into an agent room and return the reviewed result to the incumbent when needed.

This is the smallest trust boundary:

1. No incumbent access token, room history, membership list, or attachment is given to Fgentic.
1. The Matrix room has its own membership, retention, classification, agent allowlist, rate limits, and token budget.
1. An external partner joins only the purpose-scoped Matrix room; access to the incumbent does not follow.
1. The incumbent remains the source of truth for its conversations. Matrix contains only the deliberately supplied task context and agent replies.

The operational cost is a second client and deliberate context transfer. Treat that friction as a control, not as proof that a room bridge is required. Do not silently add a bot that reads every incumbent message to remove it.

### Shared IdP, separate identities

Both stacks may use the same upstream IdP, but they remain separate relying parties with separate sessions and authorization stores. Configure one client for the incumbent and another for Matrix Authentication Service (MAS):

1. Map an immutable IdP subject to each local account. Do not join accounts by display name or email address.
1. Keep group, role, guest, and deprovisioning mappings explicit per stack. A successful login does not grant room membership or agent invocation.
1. Test disable, rename, group removal, guest expiry, and break-glass administration in both systems.
1. Correlate audit records through the immutable IdP subject only where policy permits. Do not present two local user IDs as one cryptographic identity.

The reference Keycloak-to-MAS boundary is specified in [Identity and SSO](identity.md). Reusing an IdP reduces password and lifecycle duplication; it does not merge retention, audit, or authorization domains.

## Posture 2: bridge only selected rooms

A bridge expands the data boundary in both directions. It needs its own threat model, operator, database, credentials, monitoring, offboarding procedure, and acceptance evidence. Fgentic does not ship or support a Mattermost, Rocket.Chat, Zulip, or Teams room bridge today.

Before enabling a candidate, prove all of the following against the exact deployed versions:

1. **Supported access:** documented incumbent APIs, narrow service credentials, no browser-token capture, client emulation, or reverse-engineered endpoint.
1. **Identity:** stable mapping for humans, bots, guests, deactivated users, and remote Matrix users; display names are never authority.
1. **Conversation semantics:** public/private rooms, membership changes, threads, replies, edits, deletion, reactions, files, formatted text, timestamps, and backfill have documented mappings and negative tests.
1. **Delivery:** retry, deduplication, ordering, loop prevention, outage recovery, rate limits, and reconciliation after either side was unavailable.
1. **Security and operations:** maintained release, supported upgrade path, vulnerability-reporting route, credential rotation, content-safe logs, metrics, backup, and deterministic offboarding.
1. **Governance:** participants know content is copied, both retention/legal-hold regimes are reconciled, federation is either prohibited or explicitly approved, and each bridged Matrix sender is separately authorized for each agent.

If one condition fails, use the beside posture or a narrow native agent adapter. A relay that posts as one bot may still be useful for labeled notifications, but it must not be described as a room bridge.

### Current bridge evidence

This is a source snapshot reviewed on 2026-07-15, not a compatibility promise. Recheck it before opening an implementation issue.

| Incumbent        | Reviewed path                                                                                                                                                                                                                                                                                                                                        | Current conclusion                                                                                                                                                                                          |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Mattermost       | [`aiku/mautrix-mattermost`](https://github.com/aiku/mautrix-mattermost) publishes a bridgev2 implementation and `v0.3.0`, but GitHub lists one contributor and the project is new. [`hanthor/matrix-mattermost-bridge`](https://github.com/hanthor/matrix-mattermost-bridge) labels itself pre-release and says channel support still needs testing. | A credible **pilot candidate** exists, but neither project has passed Fgentic's production qualification. Do not ship or advertise either as supported.                                                     |
| Rocket.Chat      | The Matrix organization's [`matrix-appservice-rocketchat`](https://github.com/matrix-org/matrix-appservice-rocketchat) is archived and describes itself as a bare-bones text bridge for pre-enumerated channels.                                                                                                                                     | No current production candidate was found. Prefer beside or a separately scoped native Rocket.Chat app; do not fork the archived bridge into the reference platform.                                        |
| Zulip            | [`MatrixZulipBridge`](https://github.com/GearKite/MatrixZulipBridge) is an AGPL puppeting appservice with a tagged release and explicit support for streams/topics, DMs, reactions, and replies. Its own feature table also records gaps in presence, typing, formatting, media handling, and redaction direction.                                   | The strongest candidate reviewed, suitable for a controlled qualification—not a supported reference unit. Its license, secret model, semantic gaps, scale, upgrades, and offboarding still need acceptance. |
| Microsoft Teams  | [ADR 0011](adr/0011-teams-coexistence-not-bridge.md) reviews the supported Microsoft surfaces and the Matrix bridge candidates.                                                                                                                                                                                                                      | No reviewed production bridge meets the bar. Keep Teams as the worked example for choosing coexistence or, after customer validation, a Teams-native A2A adapter.                                           |
| Any of the above | [`matterbridge`](https://github.com/42wim/matterbridge) can relay Matrix with Mattermost, Rocket.Chat, or Zulip and preserves some edits, files, and threads where possible. Its latest stable release is `v1.26.0` from 2023-01-29.                                                                                                                 | A mature notification/content-relay option, not a Matrix appservice identity bridge. Qualify it for a narrow relay only; do not use it to claim full-room fidelity.                                         |

Stars, recent commits, or a successful demo do not establish production support. A production candidate needs current maintainers, supported APIs, release/security ownership, and evidence for the exact semantics above.

### Prefer a narrow native agent adapter when room mirroring is unnecessary

An incumbent-native bot can be smaller and safer than a full bridge. For example, Zulip documents an [outgoing webhook bot](https://zulip.com/help/bots-overview) that receives DMs and explicit mentions; Mattermost and Rocket.Chat expose their own bot/app surfaces. Such an adapter should send only the explicit invocation to a selected Fgentic A2A route, then return a clearly attributed bot response.

The adapter still needs an approved issue and threat-model delta. It must preserve an immutable tuple of incumbent organization, user, conversation, and message identifiers; authenticate the downstream route; enforce agent allowlists and budgets; reject bot loops; and disclose that the invocation is processed outside the incumbent. It must not invent a Matrix user or room history that does not exist.

## Posture 3: migrate human chat to Matrix

Migration removes the permanent two-client posture but is a chat-platform replacement program, not a bridge toggle. Inventory the incumbent before selecting a cutover:

1. Users, guests, teams, channels, DMs, roles, SSO/provisioning, bots, webhooks, apps, files, retention, legal holds, exports, mobile/push requirements, and compliance/audit consumers.
1. Which active rooms are recreated, which history is imported by tested tooling, and which history remains in a read-only archive. Do not promise that exports preserve every thread, edit, reaction, deletion, attachment, or original identity in Matrix.
1. Stable Matrix IDs and room aliases, MAS/IdP lifecycle, appservice namespaces, federation policy, backups, support ownership, and user training.
1. A pilot cohort, migration freeze, acceptance queries against both stores, rollback deadline, and named owner for the incumbent archive.

Federation begins only after the new Matrix rooms pass the controls in [Federation](federation.md). Migrated or bridged content does not become safe to federate merely because it is now represented as a Matrix event.

## Honest product comparison

The comparison below uses upstream product documentation reviewed on 2026-07-15. It describes licensing and architecture boundaries, not feature or support equivalence.

| Product             | Open/self-hosted base                                                                                                                                                                                                    | Agent surface and gates                                                                                                                                                                                                                                  | Federation and agent boundary                                                                                                         | What choosing Fgentic changes                                                                                                                                                                                      |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Fgentic             | Apache-2.0 project code composed with independently licensed OSS. No proprietary service or feature license is required in the critical path; [Licensing](licensing.md) records upstream AGPL and ESS Community caveats. | Matrix mentions become A2A calls through a governed gateway. Model backends and the A2A runtime are replaceable; the standalone `a2a-go` integration test enforces that boundary.                                                                        | Matrix federation is the collaboration plane; signed AgentCards plus OIDC JWT or mTLS form the cross-org delegation plane.            | Adds a protocol-defined, cross-org agent plane. It also adds Kubernetes/Flux/Matrix operations and a second client unless the organization migrates.                                                               |
| Mattermost + Agents | Mattermost documents that unlicensed Enterprise Edition is functionally equivalent to MIT Team Edition, while a license unlocks additional features. The Agents plugin source is Apache-2.0.                             | Mattermost documents one basic agent, chat, vision, and basic tools without a license; multiple agents, fine-grained controls, and self-service creation require an Entry/Enterprise tier. It supports operator-selected and self-hosted model services. | Agents are Mattermost bot identities inside the Mattermost product and permission model.                                              | Fgentic does not make Mattermost chat features better; it separates agents behind A2A and provides a Matrix-based cross-org path without making a Mattermost license or tenant the agent identity anchor.          |
| Rocket.Chat + AI    | The main repository uses MIT for the community tree and a separate license for `ee/` features.                                                                                                                           | Rocket.Chat labels its AI app beta and premium, requires an operator-deployed LLM, and directs operators to sales for enablement.                                                                                                                        | Rocket.Chat documents Matrix federation in its Government and Defense offerings; AI remains an add-on governed inside Rocket.Chat.    | Fgentic's federation and A2A delegation contracts are open reference-platform primitives rather than plan features, at the cost of operating the additional stack. Do not claim that Rocket.Chat lacks federation. |
| Zulip               | Zulip describes the self-hosted server as the same 100%-open-source software used by its cloud service, with no open-core feature split.                                                                                 | Zulip provides generic, incoming-webhook, and outgoing-webhook bots, not a first-party A2A agent-control plane.                                                                                                                                          | Zulip identities, streams, and topics remain the collaboration boundary; a third-party bridge or adapter owns any Matrix/A2A mapping. | Fgentic adds the standardized agent and federation planes, not a licensing advantage over self-hosted Zulip. Beside is usually safer than replacing Zulip solely for agents.                                       |

Primary upstream references: Mattermost [self-hosted editions](https://docs.mattermost.com/product-overview/self-hosted-subscriptions.html), [Agents license matrix](https://docs.mattermost.com/administration-guide/configure/agents-admin-guide.html#license-requirements), and [agent management gates](https://docs.mattermost.com/agents/docs/features/managing_agents.html#permissions-and-license); Rocket.Chat [plans](https://docs.rocket.chat/docs/our-plans), [AI app](https://docs.rocket.chat/docs/rocketchat-ai-app), and [repository license](https://github.com/RocketChat/Rocket.Chat/blob/develop/LICENSE); Zulip [self-hosting](https://zulip.com/self-hosting/) and [bot types](https://zulip.com/help/bots-overview).

## Decision record

Before implementation, record these answers in the deployment's design review:

1. Which named workflow cannot use a purpose-scoped Matrix agent room, and why?
1. Is the requirement agent invocation, one-way notification, or bidirectional room/history mirroring?
1. Which exact room types, events, files, identities, retention rules, and external participants are in scope?
1. Can the same IdP serve both stacks with separate clients and immutable-subject mappings?
1. May copied content federate, and have every receiving organization and retention boundary been approved?
1. Which candidate bridge version has passed the qualification checklist, and who owns its security response and upgrades?
1. What is the tested offboarding or rollback path, including credential revocation and divergent histories?

If these answers are absent, adopt beside. If they establish only explicit invocations, scope a native agent adapter. If they establish room mirroring, qualify a bridge without claiming unsupported semantics. If the organization independently wants a new human chat platform, run a migration program.
