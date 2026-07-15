---
type: Architecture Decision Record
title: Permission-Aware Retrieval Identity Binding
description: Bind database-prefiltered retrieval to a gateway-projected caller and the effective Matrix output audience.
---

# 0017 — Permission-Aware Retrieval Identity Binding

Status: Accepted

Implementation: [issue #333](https://github.com/fmind-ai/fgentic/issues/333). Until that issue ships the complete negative-test contract below, permission-aware retrieval is not an implemented platform capability.

## Context

Permission-aware retrieval must decide which chunks a caller may read before ranking or returning them. The proposed knowledge metadata carries `allowed_principals` and `allowed_groups`, but those fields are useful only when the query is bound to a trustworthy identity. D6 requires a complete Matrix user ID (MXID) such as `@alice:org-a.example`, never a localpart. D11 is equally explicit that kagent v0.9.11 accepts the caller's `X-User-Id` in unsecure mode and performs no authorization; that header is attribution, not authentication.

There are two caller classes and they do not share an identity namespace:

1. A Matrix appservice transaction has an authenticated event sender and room context at the bridge. A sender can be a native or federated Matrix user, or a local MXID reserved by an external appservice. D6 classifies the latter separately so it cannot inherit a local user's access merely because its MXID uses the local homeserver.
1. The public cross-organization A2A route authenticates an OAuth machine client. The verified tuple `(issuer, audience, azp)` identifies one partner service client under §8.3 and D7; it does not identify a Matrix user or natural person. D8 concerns loop-safe `m.notice` replies and is not an identity-mapping decision.

The pinned kagent release supplies the narrow carrier needed between those boundaries. Its [`allowedHeaders`](https://github.com/kagent-dev/kagent/blob/v0.9.11/go/api/v1alpha2/agent_types.go#L554-L566) field propagates explicitly selected A2A request headers to MCP calls. The Python executor [copies incoming headers into persistent request-session state](https://github.com/kagent-dev/kagent/blob/v0.9.11/python/packages/kagent-adk/src/kagent/adk/_agent_executor.py#L541-L554), the provider [selects only case-insensitively allowlisted names](https://github.com/kagent-dev/kagent/blob/v0.9.11/python/packages/kagent-adk/src/kagent/adk/types.py#L35-L88), and the runtime [attaches that provider to HTTP MCP tools](https://github.com/kagent-dev/kagent/blob/v0.9.11/python/packages/kagent-adk/src/kagent/adk/types.py#L397-L413). Fgentic currently forbids this field for every managed Agent; the retrieval implementation can admit one exact header for one exact Agent and tool without changing kagent.

Caller-only filtering is insufficient for a room reply. Every joined user can read a new plaintext event, and every participating homeserver operator receives federated plaintext. Allowing Alice to retrieve a chunk and then posting its derived answer where Bob cannot read the source is still a disclosure. The authorization input must include the effective output audience and the bridge must revalidate that audience when it posts the result. Here, **effective audience** means every joined identity except bridge-owned bot and ghost accounts in the trusted delivery workload; a separately operated or external-appservice bot is never excluded.

## Enforcement-point options

1. **Bridge-side retrieval before A2A.** The bridge could query the knowledge store with the Matrix sender and pass selected chunks to the Agent. This gives the bridge authoritative room context, but it does not know the model's eventual retrieval query, duplicates the retriever and audit contract, couples the generic Matrix transport to a corpus, and does not naturally cover direct cross-organization A2A. Rejected.
1. **MCP-tool-side database prefilter over a NetworkPolicy-guarded projection.** The knowledge service can apply one parameterized `WHERE` predicate before ranking, keep content and chunk ACL logic in one place, and serve the same local and cross-organization A2A path. A plain projection is not independently cryptographic after it passes through kagent, so it is accepted only from the exact retrieval-capable Agent workload through agentgateway and NetworkPolicy. Selected as the single chunk-row ACL enforcement point.
1. **agentgateway external authorization as the chunk ACL enforcement point.** agentgateway can authenticate the bridge API key or partner JWT, invoke ext-auth, and overwrite a caller header with a canonical projection. It does not own the chunk metadata or database query, however, and an A2A-entry projection must still be carried to the later MCP call. Selected only as the identity projector feeding option 2.

The minimal flow is therefore **A2A gateway projection → kagent one-header propagation → MCP gateway workload/path/tool authorization → retrieval-service decoding and database prefilter**. The bridge never performs retrieval, and no direct remote retrieval endpoint bypasses A2A and kagent. The bridge separately owns Matrix output-delivery authorization; that is not a second chunk-row ACL implementation.

## Decision

1. Protocol revisions are explicit and are not inferred from an SDK package version. The A2A hop conforms to the latest stable [A2A specification 1.0.1](https://github.com/a2aproject/A2A/blob/v1.0.1/docs/specification.md). Its selected AgentCard interface has `protocolBinding: "JSONRPC"` and `protocolVersion: "1.0"`, and service requests require `A2A-Version: 1.0`; patch versions are not negotiated, while the enclosing JSON-RPC version remains `2.0`. It uses the v1 `SendMessage`, `GetTask`, and `CancelTask` operations only where this decision permits them. The retrieval MCP hop conforms to the current stable [MCP revision `2025-11-25`](https://modelcontextprotocol.io/specification/2025-11-25/), over Streamable HTTP. Initialization must offer and negotiate exactly `2025-11-25`; every subsequent request must carry `MCP-Protocol-Version: 2025-11-25`, while an absent, different, invalid, or unsupported revision fails closed. Tool schemas use the revision's default JSON Schema 2020-12 dialect. Draft and release-candidate revisions are not implementation targets. The separately governed `kagent-tools` surface currently pinned to MCP `2025-06-18` is an older contract and cannot serve as the retrieval endpoint or its acceptance evidence.
1. The `knowledge-retrieval` service is the only chunk-row ACL enforcement point. Every search applies the validated projection inside a bound, parameterized database `WHERE` clause before similarity ranking, result counting, citation rendering, or content return. A missing, malformed, localpart-only, mixed-kind, expired, or unauthorized projection returns no chunks and no unscoped fallback query.
1. On every retrieval-Agent `SendMessage`, the A2A gateway authenticates the caller before its fail-closed external-authorization service creates one canonical `X-Fgentic-Identity` value.
   1. On the local route, strict API-key authentication must identify `matrix-a2a-bridge`. The raw pre-ext-auth header view must contain exactly one full `X-User-Id` and exactly one `X-Fgentic-Room-Context` value matching the raw Matrix schema below; either missing or duplicate header fails before decoding. Ext-auth validates the exact workload, method, Agent path, complete MXID, sender kind, and room-context schema.
   1. AgentCard discovery is not routing authority for this path. Pinned agentgateway v1.3.1 [rewrites only the legacy top-level card URL](https://github.com/agentgateway/agentgateway/blob/v1.3.1/crates/agentgateway/src/a2a/mod.rs#L75-L87), while an A2A v1 client selects `supportedInterfaces[].url`. For an explicitly configured local retrieval target, the bridge must bind the SDK transport to the configured gateway origin and exact Agent route, replacing the local copy's JSON-RPC v1 interface URL before client construction; it must never follow the upstream kagent-controller URL. Discovery metadata remains validated, but an advertised URL cannot bypass the projector. A runtime test using the real A2A SDK must observe `SendMessage` at the projected gateway listener while direct kagent is denied.
   1. On the public route, JWT authentication first validates the configured issuer, matched audience, and `azp`. Ext-auth derives identity only from that validated metadata, the exact Agent route, and one unique operator registry entry. Caller-supplied MXIDs, groups, rooms, and projections have no authority.
   1. The HTTP ext-auth configuration has a non-empty explicit request-header allowlist. It forwards only the raw identity inputs and never forwards an incoming `X-Fgentic-Identity`, API key, bearer token, cookie, or arbitrary request header; an empty configuration would trigger agentgateway's [unsafe-for-this-purpose default of forwarding `Authorization`](https://github.com/agentgateway/agentgateway/blob/v1.3.1/crates/agentgateway/src/http/ext_authz.rs#L775-L789). CEL-derived check-request headers carry the authenticated workload or validated JWT metadata to the projector.
   1. The only admitted ext-auth response headers are newly created `X-Fgentic-Identity` and `X-User-Id` values. The former is the retrieval authorization operand. The latter remains kagent attribution and session partitioning only: it is the validated full MXID for Matrix, or `oidc:<policy_id>:<identity_sha256>` for OAuth, where `identity_sha256` is lowercase SHA-256 of JCS over the exact validated `{issuer,audience,azp}` object. Pinned kagent [keys its ADK session by the derived user and A2A context](https://github.com/kagent-dev/kagent/blob/v0.9.11/python/packages/kagent-adk/src/kagent/adk/converters/request_converter.py#L11-L34). agentgateway [inserts each response header over all existing values of the same name](https://github.com/agentgateway/agentgateway/blob/v1.3.1/crates/agentgateway/src/http/ext_authz.rs#L827-L833); runtime tests retain both overwrite assertions.
   1. Authorization runs after ext-auth and requires the exact workload/client, method, Agent path, and reserved-header set. It uses the raw header view to require one final value for each projected header and rejects every unknown `X-Fgentic-*` name. A post-authorization request-header modifier explicitly removes `X-Fgentic-Room-Context` while retaining the projector-owned `X-User-Id`; no wildcard removal is assumed. This ordering matches pinned agentgateway v1.3.1: [authentication, ext-auth, authorization, then request-header modification](https://github.com/agentgateway/agentgateway/blob/v1.3.1/crates/agentgateway/src/proxy/httpproxy.rs#L143-L214).
1. The retrieval Agent propagates only `X-Fgentic-Identity` through `mcpServer.allowedHeaders`, and only to its exact `knowledge-retrieval` `RemoteMCPServer`. `X-User-Id` is consumed by kagent and never propagated as retrieval authority. The admission policy continues to reject `Authorization`, wildcard, arbitrary, and every other propagated request header. It also continues to forbid `requireApproval` for this tool: the pinned executor's [HITL-resume branch skips the normal header-state refresh](https://github.com/kagent-dev/kagent/blob/v0.9.11/python/packages/kagent-adk/src/kagent/adk/_agent_executor.py#L533-L554). Direct bridge-to-kagent routing is disabled for a retrieval-capable Agent.
1. Retrieval is stateless across delegations. Every initial retrieval `SendMessage` message must omit A2A `contextId`, `taskId`, and `referenceTaskIds`; the gateway rejects any of those fields, including an empty task-reference field. Pinned a2a-sdk v0.3.23 [generates fresh task and context IDs when both are absent](https://github.com/a2aproject/a2a-python/blob/v0.3.23/src/a2a/server/agent_execution/context.py#L173-L197), while forbidding v1 task references prevents another Agent implementation from loading earlier task context. kagent therefore cannot answer from chunks retained in an earlier room/client session after an ACL or audience change. `input-required`, `auth-required`, and HITL continuations are unsupported for the retrieval Agent and require a new independent delegation.
1. The trusted Matrix bridge may poll or cancel the server-generated task ID through its authenticated local route; those operations cannot start Agent execution or update session headers and carry no projection. The bridge starts a monotonic retrieval deadline immediately before dispatch and caps it at the earlier of its normal `TASK_TIMEOUT` and 540 seconds. That fixed ceiling is 60 seconds inside the Matrix projection's 600-second lifetime, covering the permitted 30-second clock skew plus dispatch/response margin without requiring the bridge to observe projector-owned `exp`. At the deadline the bridge stops polling, attempts cancellation, scrubs any result, and delivers no grounded content. The public OAuth route exposes neither `GetTask` nor cancellation because pinned kagent's [task store explicitly ignores call context and loads/deletes solely by task ID](https://github.com/kagent-dev/kagent/blob/v0.9.11/python/packages/kagent-core/src/kagent/core/a2a/_task_store.py#L75-L109). Public retrieval uses a single `public_retrieval_timeout_seconds` platform setting in the range 1–60, rendered into both the projector and the gateway's ingress-started static request timeout. The projector rejects a validated source token unless its remaining lifetime is strictly greater than that timeout plus the 30-second skew allowance. Only then may it emit a projection capped by the token expiry. Public `SendMessage` is synchronous with `returnImmediately` absent/false; timeout discards the downstream body, and a nonterminal result is unsupported with no follow-up route.
1. The MCP route uses a distinct backend, API-key identity, exact path, and one-tool allowlist for the retrieval Agent. Its authorization requires exactly one carrier value and rejects every other `X-Fgentic-*` name before proxying. `knowledge-retrieval` then strictly decodes the schema and validates the Agent, kind separation, expiry, group namespace, room invariants, canonical encoding, and size before constructing the database predicate. A second ext-auth hop would normalize but could not authenticate a plain value forged by the already admitted Agent pod, so it is deliberately omitted.
1. The retrieval namespace is default-deny: only the selected agentgateway proxy may reach the service, while the service may reach only DNS, its scoped knowledge database, and separately declared observability sinks. kagent and Agent pods have no direct service or database path.
1. The first implementation admits only chunks whose required `classification` metadata is `public` or `approved_non_public`, and only when that value appears in the projection's `allowed_classifications`. Missing, unknown, `restricted`, `regulated`, `secret`, and `authentication` values fail inside the same database prefilter. The bridge's exact operator-owned room registry supplies Matrix classifications; the exact OAuth client registry supplies direct-partner classifications. Both default to `public` only, and `approved_non_public` requires the corresponding room record/bilateral agreement from [ADR 0015](0015-federated-room-encryption.md) or the A2A onboarding agreement. This classification ceiling is load-bearing while the carrier remains forgeable by a compromised admitted Agent pod; #331 defines the enum at ingestion and #333 must enforce it at query time.

## Projection contract

`X-Fgentic-Identity` is unpadded base64url of UTF-8 RFC 8785 JCS JSON. JCS provides one cross-language encoding and stable digest test vectors; it is not a signature. Exactly one header value is allowed, its encoded value is at most 8,192 bytes, and its decoded JCS is at most 6,144 bytes. The decoded bytes must equal their JCS re-encoding, duplicate JSON names and unknown fields fail, and every string is valid UTF-8.

These application ceilings bound persistent session metadata independently of transport defaults. #333 must prove the 8,192-byte value through both pinned gateway hops and kagent, plus application rejection at 8,193 bytes; an incidental transport failure is not accepted as the policy control.

All projections use:

1. `v`: integer `1`.
1. `agent`: an object containing only `namespace` and `name`, each a lowercase DNS-1123 label of at most 63 bytes and derived from the exact route.
1. `allowed_classifications`: either exactly `["public"]` or the ASCII-sorted `["approved_non_public", "public"]`. No room, prompt, JWT claim, or caller input can widen the operator-owned registry value.
1. `delegation_id`: exactly 64 lowercase hexadecimal characters. Matrix uses the existing durable bridge job ID; the direct OAuth projector generates 32 random bytes. It is correlation, not replay prevention or authorization.
1. `iat` and `exp`: integer Unix seconds with `1 <= exp - iat <= 600`. Matrix uses 600 seconds and the bridge's conservative 540-second outer deadline. OAuth additionally caps `exp` at the validated source token's expiry and admits only tokens with more than the configured public timeout plus 30 seconds remaining; the matching gateway timeout therefore ends response delivery before `exp` without parsing the downstream-only carrier. Consumers allow at most 30 seconds of positive clock skew and reject when `iat > now + 30` or `now >= exp`. Every retrieval call after expiry fails closed. Expiry bounds stale persistent session state; it does not mitigate an admitted pod forging a new projection.

A Matrix projection has exactly this shape; `network` is required only when the caller kind is `bridged_matrix`:

```json
{
  "agent": { "name": "knowledge-agent", "namespace": "kagent" },
  "allowed_classifications": ["approved_non_public", "public"],
  "audience": [
    {
      "kind": "bridged_matrix",
      "network": "slack",
      "principal": "@slack-bob:org-a.example"
    },
    { "kind": "matrix", "principal": "@alice:org-a.example" }
  ],
  "delegation_id": "9f4e9e8b6a8d33bdb85d8fd5bc13ed101cb42a1c9d9b49ccf42c0a49387eddb6",
  "exp": 1784117400,
  "groups": [],
  "iat": 1784116800,
  "kind": "matrix",
  "principal": "@alice:org-a.example",
  "room": {
    "event_id": "$event",
    "history_visibility": "joined",
    "id": "!room:org-a.example",
    "join_rule": "invite",
    "state_sha256": "14e4b222c8b73ab22618ea3a9f313956fa2a81156cdaa0589e9fd3b17b25e333"
  },
  "v": 1
}
```

Matrix rules are:

1. `kind` is `matrix` or `bridged_matrix`; the latter requires one `network` DNS-1123 label of at most 63 bytes and the former forbids it. `principal` is a syntactically complete MXID of at most 255 UTF-8 bytes. The local projector classifies external-appservice namespaces from trusted bridge configuration; prompts, room state, and localparts cannot choose the kind or network.
1. `groups` is exactly an empty array in v1. Matrix authorization uses typed exact principals only, so the bridge can revalidate the audience without duplicating an entitlement registry. The matching `allowed_principals` metadata value is the same JSON object shape as an audience entry; kind and `network` are therefore part of the database comparison. Matrix group mapping requires a later ADR with one explicit source and revalidation contract.
1. `audience` contains 1–16 unique entries, includes the top-level caller, and is UTF-8 byte-sorted by `(kind, network-or-empty, principal)`. Each entry contains only `kind`, full `principal`, and conditionally `network`. It includes every effective reader and contains no bridge-owned delivery identity.
1. `room.id` and `room.event_id` are valid full Matrix room/event IDs of at most 255 UTF-8 bytes. `history_visibility` is exactly `joined`, `join_rule` is exactly `invite`, and `state_sha256` is 64 lowercase hexadecimal characters.
1. Every returned chunk must admit every audience entry through an identical object in `metadata.allowed_principals`, and its `metadata.classification` must appear in `allowed_classifications`; an empty audience, partial intersection, group-only Matrix match, or disallowed class denies the row.

The bridge supplies `X-Fgentic-Room-Context` as the same base64url/JCS encoding, with the same byte limits, containing exactly `v`, `caller`, `delegation_id`, `room`, `audience`, and `allowed_classifications`. `caller` is the same typed principal object used in `audience`; its principal must equal the incoming `X-User-Id`. `room` has the Matrix projection shape above. The bridge reads classifications only from its exact room-ID registry. The projector validates this raw object, stamps the route-derived `agent`, projector-owned `iat`/`exp`, empty `groups`, and top-level caller fields, then emits the canonical projection and overwrites downstream `X-User-Id` with that validated principal.

An OAuth projection has exactly this shape:

```json
{
  "agent": { "name": "knowledge-agent", "namespace": "kagent" },
  "allowed_classifications": ["approved_non_public", "public"],
  "client": {
    "audience": "fgentic-a2a",
    "azp": "org-b-a2a",
    "issuer": "https://issuer.example",
    "policy_id": "org-b"
  },
  "delegation_id": "33e24ba4e2eef670a7983e8a9bc77d110111b54e21179079807df031c2b877b1",
  "exp": 1784117400,
  "groups": ["partner/org-b/docs"],
  "iat": 1784116800,
  "kind": "oidc_client",
  "v": 1
}
```

OAuth rules are:

1. One exact, unique operator registry entry keyed by `(issuer, matched configured audience, azp)` supplies `policy_id`, groups, and `allowed_classifications`. `issuer` is at most 512 bytes and exact-match only. `audience` and `azp` are at most 255 bytes. `policy_id` is one DNS-1123 label.
1. There are 1–16 ASCII-sorted unique groups. Each is exactly `partner/<policy_id>/<group-id>`, where both variable segments are DNS-1123 labels. No partner group can equal or imply a local group.
1. The object contains no MXID, Matrix room, top-level Matrix audience list, network, or local group. Retrieval authorizes it only through the projected partner groups and allowed classification, never by comparing a bare `azp` with `allowed_principals`.
1. A returned chunk's `metadata.classification` must appear in `allowed_classifications`, and at least one projected group must exactly equal one string in `metadata.allowed_groups`. An absent or empty row group list, no exact intersection, a prefix match, or any `allowed_principals` entry alone denies an OAuth row.

## Matrix snapshot and delivery

Immediately before A2A dispatch, the bridge reads only the authorization-relevant current state from Synapse: the room-create, joined-member, `m.room.join_rules`, `m.room.history_visibility`, and, when federated, `m.room.server_acl` events. It rejects incomplete reads, requires invite-only/joined-only state, resolves every joined identity kind, and requires the caller and target ghost to remain joined. Room-v12 and federation-border acceptance remain separate onboarding/configuration evidence under ADR 0015; the client-state read does not pretend to discover a portable room-version value.

The bridge computes `state_sha256` as lowercase SHA-256 of UTF-8 JCS over this exact object:

```json
{
  "allowed_classifications": ["approved_non_public", "public"],
  "create": { "event_id": "$create", "federate": true },
  "history_visibility": { "event_id": "$history", "value": "joined" },
  "join_rule": { "event_id": "$join", "value": "invite" },
  "joined": [
    {
      "event_id": "$member-agent",
      "kind": "bridge_service",
      "principal": "@agent-docs:org-a.example"
    },
    {
      "event_id": "$member-slack-bob",
      "kind": "bridged_matrix",
      "network": "slack",
      "principal": "@slack-bob:org-a.example"
    },
    {
      "event_id": "$member-alice",
      "kind": "matrix",
      "principal": "@alice:org-a.example"
    }
  ],
  "room_id": "!room:org-a.example",
  "server_acl": {
    "allow": ["org-a.example", "org-b.example"],
    "allow_ip_literals": false,
    "deny": [],
    "event_id": "$acl"
  }
}
```

`joined` contains every current joined member, including excluded `bridge_service` identities, and is UTF-8 byte-sorted by `(kind, network-or-empty, principal)`; `network` appears only for `bridged_matrix`. `create.federate` normalizes the Matrix default (`true` when `m.federate` is absent). The ACL object may be `null` while every joined identity is local to the homeserver; it is required and policy-valid as soon as a native Matrix member from another homeserver is joined. Otherwise its `allow` and `deny` strings are sorted/deduplicated and `allow_ip_literals` is explicit. A missing current state event is represented only where this contract permits `null`; it is never silently replaced with another default.

The displayed snapshot's JCS bytes hash to `14e4b222c8b73ab22618ea3a9f313956fa2a81156cdaa0589e9fd3b17b25e333`, the value used in the Matrix projection example. This pair is the cross-language contract test vector.

Immediately before every content-bearing Matrix send, retry, edit, or artifact post, the bridge repeats those reads, room-classification lookup, policy checks, audience derivation, and digest computation. Any membership, history-visibility, join-rule, server-ACL, federation, identity-kind, allowed-classification, or digest drift—including removal—suppresses and scrubs the persisted result, emits content-free denial evidence, and requires a fresh delegation. A bounded generic notice may ask the caller to retry but may not contain grounded content. The state read and Matrix send are not atomic; Matrix offers no conditional send tied to a room-state digest, so that remaining time-of-check/time-of-use window is explicit.

A direct `oidc_client` request has no trustworthy Matrix room audience. Its output audience is the verified service client and mapped partner policy only; org A does not accept a partner-asserted remote room snapshot. Redistribution by that client remains the partner trust boundary.

## Residual trust and re-evaluation

1. `X-Fgentic-Identity` is not end-user authentication performed by kagent. Its authenticity rests on the authenticated A2A gateway, exact workload secrets, fail-closed ext-auth/CEL policy, the one-header admission rule, and enforced NetworkPolicies. An admitted or compromised retrieval Agent pod that can reuse its own MCP credential can forge a syntactically valid plain projection; NetworkPolicy authenticates no process. The expiry limits stale honest session state, not that forgery risk. This residual and the global classification ceiling are deliberate v1 constraints.
1. kagent persists the carrier in session state, so even the bounded Matrix audience is personal/security metadata subject to the kagent database and backup retention boundary. The projection carries no prompt, query, chunk, Matrix display name, or Matrix group entitlement. It must not be logged. Fresh server-generated contexts without caller task/context/reference IDs prevent conversational/tool results crossing delegation boundaries; #333 must document retention and prove that an earlier grounded result cannot appear in a later delegation after audience, ACL, or classification drift.
1. Adopt a signed or stateful capability through a follow-up ADR before admitting `restricted`, `regulated`, `secret`, or `authentication` corpus classes, adding more retrieval-capable workloads, or accepting a target where the workload/NetworkPolicy boundary cannot be proved. A `jti` without verifier state provides correlation rather than replay prevention, while one-shot consumption would break legitimate multiple retrieval calls in one delegation.
1. D11 remains globally true. Re-evaluate when kagent [#1270](https://github.com/kagent-dev/kagent/issues/1270), or an accepted successor, ships non-prerelease per-Agent authorization and kagent [#1890](https://github.com/kagent-dev/kagent/issues/1890), or an accepted successor, supplies a stable A2A external-authorization hook that Fgentic can configure end to end. Native kagent controls may replace part of the workload boundary; they do not remove database prefiltering or room-audience authorization.
1. Matrix membership can change after the final read, an admitted homeserver operator can read federated plaintext, and already delivered content cannot be recalled. One-principal rooms, conditional Matrix sends, or per-recipient encryption would narrow those limits; none exists in the current bridge contract. ADR 0015's data classification and bilateral controls remain load-bearing.

## Consequences

1. One query implementation and audit boundary serve local Matrix, federated Matrix, bridged-appservice, and direct partner A2A identities without pretending they are interchangeable.
1. The bridge gains bounded room-state snapshot and final-delivery checks but not corpus, query, ranking, group mapping, or ACL storage. This stays inside [ADR 0012](0012-bridge-decomposition-surface-budget.md)'s Matrix delegation and output responsibilities; the independently scalable retrieval service remains a sibling app.
1. Permission-aware retrieval becomes unavailable when identity projection, the required room-state reads, agentgateway, or NetworkPolicy enforcement is unavailable or ambiguous. That availability cost is preferable to an unscoped query or room disclosure.
1. Multi-user rooms use the intersection of every current reader's exact-principal permissions. They can therefore return fewer chunks than the initiating caller could read privately; operators can use purpose-specific rooms rather than weakening the predicate. Retrieval Agents also give up cross-delegation conversational memory to prevent previously grounded content bypassing a later ACL decision.
1. Implementation acceptance must include exact A2A 1.0 and MCP `2025-11-25` negotiation plus rejection of missing/wrong subsequent MCP version headers; real-SDK proof that a v1 local AgentCard cannot select the direct kagent URL; missing/duplicate raw local identity headers; missing/duplicate/oversized/non-canonical/expired projections; a future `iat`; localpart-only MXID; mixed identity kinds; wrong Agent/key/path/tool; unknown or overlapping partner mappings; partner/local group collision; absent/empty/non-intersecting/prefix-only OAuth row groups; forbidden or room/client-disallowed corpus classifications; direct kagent/retrieval/database bypass; unauthorized audience members; caller-supplied context/task/reference IDs; two clients attempting the same context; earlier grounded content after audience/ACL/classification drift; HITL/input continuation admission; forbidden public task read/cancel; public nonterminal results; near-expiry OAuth tokens and mismatched/invalid public timeout settings; explicit ext-auth header allowlisting and credential/cookie non-forwarding; state drift before every delivery form; exact header-boundary values across both pinned hops; and installed-CNI NetworkPolicy negatives.
