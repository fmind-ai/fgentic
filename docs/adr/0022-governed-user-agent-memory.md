---
type: Architecture Decision Record
title: Governed User-Agent Memory Requires Explicit Consent and Exact Erasure
description: Keep long-term semantic memory disabled until one user-agent scope can be inspected, bounded, and erased without leaking across rooms or federation.
---

# 0022 — Governed User-Agent Memory Requires Explicit Consent and Exact Erasure

Status: Proposed

Implementation: [issue #193](https://github.com/fmind-ai/fgentic/issues/193). This proposal does not enable memory or authorize implementation issues. A maintainer must first accept, amend, or reject the privacy stance.

## Context

Fgentic already has **conversation memory**: the bridge reuses one A2A `contextId` per `(room, ghost)`, and kagent persists the corresponding session. [Issue #100](https://github.com/fmind-ai/fgentic/issues/100) added `!forget <agent>` and `maxSessionAge` for that session. Those controls do not govern kagent's separate long-term semantic memory store.

Pinned kagent `v0.9.11` can extract and retain facts across sessions. Its [memory design](https://github.com/kagent-dev/kagent/blob/v0.9.11/design/EP-1256-memory.md) and exact source establish the current boundary:

1. Memory is keyed by `agent_name` and `user_id`, stored as plaintext content plus a 768-dimensional embedding in kagent's PostgreSQL database. The bridge currently supplies the complete Matrix sender MXID as `X-User-Id`, but D11 classifies that value as attribution rather than global authentication.
1. The runtime automatically summarizes and saves a session every fifth user turn, may save facts through `save_memory`, and prefetches relevant facts into a later session. Enabling the Agent-level setting therefore enables collection for every admitted user; it is not a per-user consent switch.
1. `GET /api/memories` returns every memory and its content for one `(agent_name, user_id)`. `DELETE /api/memories` hard-deletes only the complete set for that pair. The pinned [handler](https://github.com/kagent-dev/kagent/blob/v0.9.11/go/core/internal/httpserver/handlers/memory.go#L246-L298) has no per-entry delete operation.
1. `ttl_days` is not a strict maximum retention period. The pinned [database queries](https://github.com/kagent-dev/kagent/blob/v0.9.11/go/core/internal/database/queries/memory.sql) extend an expired, frequently accessed memory by 15 days and reset its access count; only less-accessed expired rows are deleted.
1. Summarization, embeddings, prefetch, and explicit load/save consume model or embedding capacity outside the bridge's current invocation budget. D7's chat-token controls do not by themselves bound those background operations.

A raw `(agent, MXID)` store also crosses Matrix disclosure boundaries. The same person may invoke an Agent in a private room, a team room, and a federated partner room. Retrieving a private preference into a plaintext group reply exposes it to every current room reader and participating homeserver, even if the original fact was collected elsewhere. A localpart, email address, or unverified cross-platform handle cannot safely merge those identities.

The placement choices are:

1. **Expose kagent memory directly.** This is the smallest implementation, but it makes consent Agent-wide, cannot erase one fact, does not enforce a strict retention ceiling, and can reuse facts across rooms. Rejected.
1. **Store memory content in the bridge database.** This permits exact policy but makes the Matrix transport own sensitive semantic content, embedding/search, and a new data lifecycle. It violates the surface budget in [ADR 0012](0012-bridge-decomposition-surface-budget.md) and duplicates kagent. Rejected.
1. **Keep kagent as the content store, add the missing governed contract, and let the bridge own only Matrix consent/control UX.** Selected. Required kagent capabilities may be contributed upstream. If upstream cannot provide them, a separately reviewed sibling memory service is the escape hatch; it is not silently folded into the bridge.

## Decision

1. **Long-term semantic memory remains disabled by default.** No default, demo, federation, or production-shaped profile enables kagent `memory` until every gate in this decision is implemented and tested. A Proposed ADR is not permission to collect memory.

1. **The authorization key is a typed triple:** complete user principal, exact Agent identity, and memory scope.
   1. A Matrix principal is the complete MXID plus its origin kind; a localpart alone is invalid. An external-appservice identity remains distinct from a native local identity.
   1. The default memory scope is one explicitly managed single-subject control room. It never inherits an ambient room, Space, homeserver, or localpart namespace.
   1. Cross-room personal memory is a later opt-in scope. It requires an authenticated canonical-person binding and an output-audience policy at least as strict as every source scope. Matching email, display name, localpart, or Fediverse handle is not sufficient.
   1. A memory-enabled delegation fails closed when the current room, principal, Agent, classification, or output audience does not exactly match the consented scope.

1. **Consent precedes collection and retrieval.**
   1. Consent is explicit, purpose-specific, versioned, and off by default for each typed scope. Room membership, invoking an Agent, or accepting general terms is not memory consent.
   1. Before consent, neither automatic save, `save_memory`, prefetch, nor load may run. Because pinned kagent enables these at the Agent level, an implementation must add a per-request/per-scope disable gate or route non-consenting requests to a memory-disabled runtime; prompts alone are not enforcement.
   1. Every memory read and write carries the current consent generation. The content-store boundary transactionally compares that generation with a durable scope record and rejects a missing, disabled, expired, or stale generation. This fence applies to automatic background saves and explicit tools, not only requests initiated by the bridge.
   1. Withdrawal atomically disables the scope and advances its generation before waiting for or rejecting in-flight work. Erasure then follows a fixed **disable/fence → quiesce-or-reject stale work → delete → read-after-delete verify** order. A background save scheduled under the old generation can never recreate memory after verification.
   1. If fencing, erasure, or verification fails, the scope remains disabled and reports a content-free operator failure; it never resumes on a best-effort assumption.

1. **User controls are exact and private.** The initial Matrix contract is:
   1. `!memory status <agent>` shows whether the sender's exact scope is enabled, its purpose, hard expiry, entry cap, and last verified erasure state.
   1. `!memory inspect <agent>` returns bounded, paginated entries with stable opaque ID, stored text, source scope, creation time, and hard expiry.
   1. `!memory forget <agent> <entry-id>` deletes exactly one inspected entry and verifies that it is no longer readable.
   1. `!memory forget <agent> all` and `!memory off <agent>` require an explicit confirmation; `off` disables future collection before clearing all entries.
   1. Only the exact subject may inspect or erase personal memory. A room moderator or platform operator cannot retrieve its content through the Matrix UX.
   1. Commands execute and reply only in a room-v12, unencrypted, invite-only, joined-history control room whose exact allowed state is the subject, the bridge-owned bot, and the selected local Agent ghost, with no other joined or invited identity and a server ACL admitting only the local homeserver. The bridge checks the complete current membership, join rule, history visibility, encryption absence, and ACL at command admission.
   1. The bridge recomputes that state immediately before every content-bearing response, retry, and paginated inspect page. Any state drift suppresses the content and fails closed with a content-free notice. A previously delivered Matrix event remains subject to normal history, device-cache, retention, and backup limits.

1. **kagent remains the content store; the bridge stores control state only.**
   1. Memory text, embeddings, and search results stay in kagent's scoped PostgreSQL database. They never enter bridge tables, logs, metrics, traces, audit events, or Matrix room state.
   1. The bridge may persist a content-free consent receipt, typed scope, policy version, entry/operation identifiers, timestamps, and terminal result. That record is governance evidence, not memory content.
   1. Before implementation, the kagent boundary must support a scope distinct from attribution, consent-generation fencing at every read/write, per-entry hard deletion with read-after-delete verification, read-time expiry enforcement, an immutable maximum expiry, and a hard per-scope entry cap. Pinned `v0.9.11` does not satisfy that contract.
   1. Expiry is an authorization boundary, not a pruning hint. Every list, search, prefetch, inspect, and Agent retrieval query requires `expires_at > now` at the content-store boundary. At the instant that predicate becomes false the entry is unreadable, regardless of cleanup state or access count. Physical deletion follows within 24 hours and cannot extend the expiry; cleanup lag is not permission to return content.
   1. The controller API remains cluster-internal behind exact workload and NetworkPolicy authorization. D11's unauthenticated kagent endpoint is never exposed to Matrix clients or federation peers.

1. **Federation is denied by default.**
   1. The first implementation admits only a native local MXID in the local-only single-subject control room defined above.
   1. A remote Matrix user, bridged identity, OAuth client, or Fediverse actor cannot store or retrieve personal memory until a reviewed bilateral policy names the data controller/processor roles, purpose, classification, residency, retention, erasure owner, and canonical principal binding.
   1. Memory is never injected into a federated room merely because the subject is local. Matrix replication makes every participating homeserver an output recipient; the scope must authorize that audience explicitly.
   1. Offboarding one partner disables affected scopes before trust removal and records the remaining backup/federated-copy limits. Trust revocation does not retract previously replicated Matrix history.

1. **Conversation reset, semantic-memory erasure, and identity offboarding remain separate operations.**
   1. `!forget <agent>` from #100 resets the room/Agent kagent session. It does not imply semantic-memory deletion.
   1. `!memory forget` and `!memory off` govern semantic memory. They do not redact Matrix events, media, model-provider records, or backups.
   1. The offboarding reconciler in [#153](https://github.com/fmind-ai/fgentic/issues/153) must disable memory before MAS deactivation, enumerate every consented Agent/scope for the exact principal, clear it, and retain a content-free result. GDPR erasure still composes with the wider [retention runbook](../retention.md) and its stated residuals.

1. **The first prototype is deliberately narrow and cost-bounded.**
   1. One sample Agent, one native local test user, and one local-only single-subject control room are enabled on the disposable demo profile only. No remote identity, group room, default profile, or production claim is included.
   1. Embeddings use the opt-in self-hosted BGE-M3 profile through the governed agentgateway path. No paid provider or credential-bearing direct model route is introduced.
   1. The candidate policy uses a strict seven-day maximum, at most 100 entries per scope, and at most 512 UTF-8 bytes per stored fact.
   1. Cost admission is enforced before model or embedding work. One new session may issue at most eight prefetch subqueries of at most 512 UTF-8 bytes each. Explicit load/save operations share the bridge's six-per-minute, burst-three sender/Agent ceiling and also have a 60-operation per-scope hourly ceiling. One automatic save may run per five user turns; its input is capped at 16 KiB and its output at ten facts. Every summarization, prefetch subquery, explicit load/save, and embedding item consumes the hourly ceiling. Over-limit memory work fails closed without blocking an otherwise memory-free chat reply.
   1. The implementation meters admitted embedding, summarization, save, and retrieval operations separately from chat tokens and exports bounded aggregate counters only. Metering is evidence after admission, not the limit itself; reservations and operation counts are never reported as model-token consumption.
   1. Acceptance proves no collection before consent; private inspect; exact one-entry erase; disable-and-clear; hard expiry; entry-cap refusal; no cross-room, cross-user, cross-agent, bridged, or federated reuse; and offboarding enumeration. A test prompt that asks the Agent to ignore these rules must not bypass them.

1. **Implementation work is cut only after human privacy approval.** The approved sequence is expected to be:
   1. upstream or adapter capability: typed scope, consent-generation fencing, read-time expiry, per-entry delete, immutable expiry, cap, and per-request disable;
   1. bridge consent/control UX and content-free evidence;
   1. one disabled-by-default sample Agent/profile with self-hosted embeddings and operation metering;
   1. deterministic negative/runtime proofs, including federation denial; and
   1. #153 offboarding integration.

   These are design slices, not pre-authorized issues. The maintainer approves the privacy stance before implementation issues are created.

## Consequences

1. Fgentic does not claim that an Agent "remembers the user" until the user can see, bound, and erase what is retained. The competitive feature ships later, but with an auditable privacy contract rather than an irreversible default.
1. The bridge owns the interaction and consent boundary without becoming a vector database or semantic-content custodian. kagent remains replaceable behind a narrow governed contract.
1. Pinned kagent needs upstream work or a separately reviewed adapter before the proposal is implementable. That dependency is intentional: unfenced background writes, whole-store deletion, read-visible expired rows, and popularity-extended TTL cannot be presented as reliable `off`, exact "forget X", or maximum retention.
1. The first release does not provide cross-platform or federated personal memory. Those features require canonical identity and output-audience decisions that do not exist today.
1. Inspection necessarily returns sensitive content to Matrix. Restricting it to a managed single-subject control room reduces exposure but does not make Matrix history erasable; the normal retention, backup, device-cache, and federation caveats still apply.
1. The escape hatch is a self-contained sibling memory service with its own database, API, NetworkPolicy, and lifecycle. Choosing it requires a follow-up ADR and explicit path ownership; it is not an excuse to grow the bridge or bypass upstream governance.
