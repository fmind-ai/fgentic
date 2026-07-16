---
type: Runbook
title: Delegation Attribution Audit
description: Evidence chains for Matrix delegation, database changes, and Kubernetes control-plane actions.
---

# Delegation Attribution Audit

This runbook answers five questions for one Matrix delegation: **who** invoked an agent, **what** target and exact agent version were invoked, **when** it ran, and **what model usage** the task consumed. The join key starts with the Matrix event ID and ends with the kagent task ID.

The current platform can prove the Matrix identity asserted through the bridge and persisted by local kagent. For configured remote targets, it can additionally prove that a fetched AgentCard matched the pinned A2A v1.0 ES256 identity before dispatch; an untrusted-card refusal has no downstream task evidence because dispatch never occurred. A successful remote delegation stops at the bridge's A2A IDs in local evidence—the partner must supply its own task, model, and token-usage records, and the kagent collector below is therefore local-target-only. The platform cannot prove that the same human controlled the Matrix account, authenticate that identity again at kagent, or deterministically attach an agentgateway request to one kagent task. Exact per-task token usage is available from local kagent; exact currency cost is not available until a versioned agentgateway cost catalog and cross-hop correlation are configured. Those are evidence limits, not implied guarantees.

## Evidence chain

```text
Matrix event_id
  ├─ appservice transaction ID + exact-body SHA-256
  └─ sender + room + origin_server_ts
       └─ durable job ID (event ID + target ghost)
            ├─ checked ledger transitions + lease generation
            ├─ deterministic A2A/Matrix IDs
            └─ bridge terminal audit record (fgentic.delegation.v1)
                 ├─ sender origin kind/network + agent path
                 ├─ agent version ── live mapping + Flux Git revisions
                 ├─ X-User-Id assertion
                 ├─ A2A contextId ── kagent session.user_id
                 └─ A2A task ID ──── kagent task metadata + exact token usage
                                           ⇢ agentgateway request logs by time/model/tokens
                                           ⇢ Prometheus aggregate counters
```

| Hop                  | Evidence and join fields                                                                                                                                                                                                                                     | What it proves                                                                                                                                                                                                                                                                                                                                          |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Matrix / Synapse     | `event_id`, `sender`, `room_id`, `origin_server_ts`, event content                                                                                                                                                                                           | The homeserver accepted an event from an authenticated Matrix session for that MXID. Matrix is the source of record for the requested content.                                                                                                                                                                                                          |
| Bridge ledger        | `bridge_appservice_transactions`: transaction ID, exact-body SHA-256, commit time. `bridge_delegations`: deterministic job ID, event/ghost, checked state, lease generation, attempts, stable reason, A2A IDs, Matrix transaction/event IDs, and timestamps. | The transaction and every eligible target were accepted before HTTP 200; whether work is pending, safely resumable, awaiting Matrix projection, terminal, or acknowledgement-ambiguous; and which downstream identities are bound to it. Active content columns are deliberately excluded from this evidence projection.                                |
| Bridge conflict log  | JSON `msg="appservice transaction conflict"`, `log_stream="audit"`, `audit_schema="fgentic.appservice_transaction.v1"`: transaction ID, rejected outcome/reason, and stored/received exact-body SHA-256 hex                                                  | A changed body reused an already-committed transaction ID and was rejected before mautrix consumption or job creation. It does not identify who or what caused the mismatch.                                                                                                                                                                            |
| Bridge terminal log  | JSON `msg="delegation audit"`, `log_stream="audit"`, `audit_schema="fgentic.delegation.v1"` records: Matrix event/sender/room, bounded sender-origin kind/network, target, agent version, A2A IDs, outcome/reason, duration, and rate-limit verdicts         | Which exact effective Agent contract and mapping the bridge selected, whether a configured external appservice namespace classified the sender, which value it asserted in `X-User-Id`, and the terminal result. Prompts and message bodies are intentionally absent. Treat the database ledger, not pod-log survival, as the recovery source of truth. |
| kagent session store | Session `id`, `user_id`, `agent_id`; task `id`, `contextId`, state/timestamp; `kagent_user_id`, `kagent_invocation_id`, `kagent_usage_metadata`                                                                                                              | kagent persisted the asserted user under the same context and task. Task usage metadata is the exact per-task token evidence emitted by the agent runtime.                                                                                                                                                                                              |
| agentgateway logs    | Request time, route, provider, requested/response model, HTTP status, input/output tokens                                                                                                                                                                    | A model request crossed the governed egress path. The current kagent model call does not propagate Matrix, context, task, or invocation identity, so time/model/token matching is corroboration only.                                                                                                                                                   |
| MCP tool-call log    | `audit.kind=fgentic.mcp_tool_call.v1`, authenticated Agent, method, resolved tool/target, quota policy class, HTTP status, duration                                                                                                                          | One governed tool call was admitted or denied at agentgateway. HTTP 429 proves a quota admission denial, not tool execution, model-token use, remaining quota, or billable consumption. Arguments, results, credentials, and quota values are absent.                                                                                                   |
| Prometheus           | `fgentic_delegations_total{ghost,outcome}`, `fgentic_delegation_ledger_transitions_total{from_state,to_state}`, `fgentic_delegation_recovery_outcomes_total{outcome}`, and `agentgateway_gen_ai_client_token_usage_sum` with provider/model/token dimensions | Aggregate delegation, workflow, operator-attention recovery, and model consumption. Identity, room, context, and task labels are deliberately excluded to avoid personal-data and cardinality hazards.                                                                                                                                                  |
| PostgreSQL / pgAudit | CNPG JSON stdout with `logger=pgaudit`, `msg=record`, database/session role, and `record.audit` class, command, object type/name, and statement IDs                                                                                                          | A `DDL` or `ROLE` operation reached PostgreSQL under one database session role. This is an independent database-change hop, not a join to the Matrix delegation; SQL text and parameters are explicitly suppressed.                                                                                                                                     |

The bridge sends `X-User-Id: <sender_mxid>` on A2A AgentCard, `SendMessage`, and `GetTask` requests. The in-process contract test `TestClientContract_MessageContextAttributionAndWireVersion` is the wire-level proof. kagent 0.9.11 reads this header in its [unsecure authenticator](https://github.com/kagent-dev/kagent/blob/v0.9.11/go/core/internal/httpserver/auth/authn.go), then persists it with the context/session. Its runtime explicitly marks that identity as unauthenticated. NetworkPolicy is therefore the compensating boundary described in the [threat model](security.md).

The deterministic A2A message ID proves which request identity the bridge attempted; it does not prove that the target deduplicates it. If HTTP transport may have started but no acknowledgement returned, the job becomes `ambiguous` and is not resent. If an acknowledgement supplied a task ID, recovery uses `GetTask`; if an agent result was already persisted, recovery projects the same `reply_pending` payload through its deterministic Matrix transaction ID. Conversation-context and job evidence update atomically.

The PostgreSQL row is deliberately orthogonal to the delegation chain. It can corroborate a schema or privilege change performed by a service/database role, but it carries no Matrix event, task, workload, or human identity and must not be presented as delegation attribution.

The MCP record is likewise an independent capability-use record. Select it by `audit.kind`, authenticated Agent, resolved tool, status, and time window; do not infer which Matrix event caused it because the current MCP hop has no event, room, sender, context, or task join key. `mcp.quota.policy=per_agent_and_tool_admission` states which admission class was checked. A 429 means at least one configured fixed-window ceiling denied the request, but the record intentionally does not reveal which descriptor, its threshold, current counter, or remaining capacity. A non-429 record does not prove successful tool execution, and none of these fields are consumption or billing evidence.

## Cross-organization usage receipt

The federation lab's inbound public docs-qa route returns a seller-signed, content-free `fgentic.usage-receipt.v1` under `https://fgentic.fmind.ai/a2a/extensions/usage-receipt/v1` in terminal Task metadata: `result.task.metadata` for a wrapped `SendMessage` Task, or direct `result.metadata` for `GetTask`. A direct A2A Message is not terminal-task evidence and fails closed instead of being relabeled as completed. It is a separate evidence chain from the Matrix bridge audit above: the authenticated subject is the org-B machine client `azp`, not a Matrix sender or natural person.

| Field                                         | Evidence meaning                                                                                                                |
| --------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `azp`                                         | The exact authorized-party claim already validated by agentgateway before the receipt processor ran.                            |
| `taskId`, `contextId`, `outcome`, `timestamp` | The terminal A2A identity and seller-observed completion state/time.                                                            |
| `requestHash`                                 | SHA-256 of the RFC 8785 canonical A2A request; it binds the receipt without retaining prompt content.                           |
| `tokensReserved`                              | The caller-declared ceiling admitted against that `azp`'s quota. It is not consumption.                                         |
| `tokensConsumed`                              | Always `null` until the model boundary can attribute provider-reported actuals to this consumer and task.                       |
| `keyId`, `protected`, `signature`             | The AgentCard-pinned key identity and flattened ES256/JCS proof. The receipt key is independent from the AgentCard signing key. |

The processor appends the signed JSON object to its single-writer JSONL archive before adding it to the response. For a working Task, it persists only the request hash and reservation and correlates them with a later authenticated `GetTask`; it stores no request or response body. Repeated terminal `GetTask` calls replay the exact archived signature, so a lost client response does not mint a duplicate assertion or make the receipt unrecoverable. Verify a returned object with the separately authenticated public JWK published in `federated-docs-qa-agent-card`:

```bash
scripts/usage-receipt.sh verify \
  --input usage-receipt.json \
  --public-key usage-receipt-public-jwk.json \
  --key-id fgentic-org-a-usage-receipt-v1
```

A valid signature proves that org A's receipt key signed those bounded fields. It does not prove natural-person identity, actual token consumption, price, currency cost, payment, or that a partner retained the receipt. An unauthorized prompt cannot manufacture evidence: JWT and exact-route authorization execute before the private external processor, and the lab compares the archive count across negative probes.

## ActivityPub transport evidence chain

The ActivityPub agent gateway (`apps/activitypub-agent-gateway`, the second federation transport — [ADR 0014](adr/0014-activitypub-second-federation-transport.md), [fediverse spec §3](fediverse.md)) mirrors the bridge's evidence contract for fediverse-originated delegations. The join key starts at the inbound activity's actor URI, which the F3/F4 border **verified** (HTTP Signature + git allowlist + FEP-8b32 object integrity) before any A2A call.

| Hop                     | Evidence and join fields                                                                                                                                                                                      | What it proves                                                                                                                                                                   |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Remote instance / actor | The signed inbound `Create(Note)`: actor URI, HTTP-Signature keyId, FEP-8b32 `proof`                                                                                                                          | A remote instance signed the activity and its object. The border admitted it only if the signature bound to the actor, the actor was allowlisted, and the object proof verified. |
| AP gateway audit record | JSON `msg="delegation audit"`, `log_stream="audit"`, `audit_schema="fgentic.ap.delegation.v1"`: `ghost`, `a2a_user_id` (full actor URI), `origin_kind=activitypub`, `origin_network`, `context_id`, `outcome` | Which agent was selected, the authoritative actor URI asserted downstream, the bounded origin, and the terminal result. Note content and token figures are intentionally absent. |
| A2A request headers     | `X-User-Id: <full actor URI>` plus bounded `X-Origin-Kind: activitypub` and `X-Origin-Network: <signing domain>`                                                                                              | The gateway forwarded the verified actor verbatim. `TestAttributionHeaders` + `TestDelegationEmitsAuditWithFullActorURI` are the wire-level proofs.                              |
| kagent / agentgateway   | Same as the Matrix chain below (session `user_id` = actor URI; aggregate GenAI token metering)                                                                                                                | kagent persisted the asserted actor under the context; model consumption stays aggregate at agentgateway.                                                                        |

**The full actor URI is authoritative and is never dropped or shortened.** `origin.kind` and `origin.network` (the signing domain) are **bounded, additive** audit fields — the exact parallel of the bridge's `sender_origin_kind`/`network` for external-appservice MXIDs — and never replace the URI in the A2A call or logs. The per-actor/domain token budget is admission accounting (a reservation, [D8](design-decisions.md)); it is never emitted as consumption and never labelled by remote actor.

**Honest machine labeling.** Every agent actor is `type: Service` with `bot: true`, so a Fediverse client cannot mistake an agent for a person. Agent replies are authored by that Service/bot actor: they are the ActivityPub equivalent of the bridge's Matrix `m.notice` — **automation output that other automation MUST NOT act on**. There is no AP `m.notice`; the Service/bot actor typing carries this non-actionable contract.

## Collect one evidence bundle

Prerequisites are a reconciled cluster, the audited bridge build, the repository's pinned `mise` toolchain, `kubectl` access to the five namespaces, and `jq`. Keep the event ID in single quotes because Matrix event IDs begin with `$`.

1. Send one unique, single-target `@agent-…` mention and retain the event ID returned by the Matrix client API. Do not put secrets or regulated data in an audit probe. A multi-agent event correctly produces one bridge record per target, so the collector rejects it as ambiguous.
1. Collect the content-free bundle while the pod logs are still available:

   ```bash
   EVENT_ID='$replace-with-the-matrix-event-id'
   mise exec -- scripts/audit-attribution.sh "${EVENT_ID}" 15m > audit-evidence.json
   ```

1. Require every deterministic join to match:

   ```bash
      jq -e --arg event_id "${EVENT_ID}" '
        .bridge.matrix_event_id == $event_id
        and (.bridge.dedup_verdict == "accepted" or .bridge.dedup_verdict == "check_error")
        and .bridge.rate_limit_verdict == "allowed"
        and .bridge.a2a_attempted
     and .bridge.sender_mxid == .bridge.a2a_user_id
     and .bridge.a2a_context_id == .kagent.session.session.id
     and .bridge.sender_mxid == .kagent.session.session.user_id
     and .bridge.a2a_task_id == .kagent.task.id
     and .bridge.a2a_context_id == .kagent.task.contextId
     and .bridge.sender_mxid == .kagent.task.metadata.kagent_user_id
     and .bridge.a2a_context_id == .kagent.task.metadata.kagent_session_id
   ' audit-evidence.json
   ```

1. Read the compliance summary without exposing the Matrix message body:

   ```bash
   jq '{
     who: .bridge.sender_mxid,
     origin: {
       kind: .bridge.sender_origin_kind,
       network: .bridge.sender_origin_network
     },
     what: {
       matrix_event_id: .bridge.matrix_event_id,
       room_id: .bridge.room_id,
       ghost: .bridge.ghost,
       ghost_mxid: .bridge.ghost_mxid,
       agent_path: .bridge.agent_path,
          agent_version: .bridge.agent_version,
          agent_contract_sha256: .bridge.agent_contract_sha256,
          task_id: .bridge.a2a_task_id,
          outcome: .bridge.outcome,
          terminal_reason: .bridge.terminal_reason,
          duration_ms: .bridge.duration_ms,
          dedup_verdict: .bridge.dedup_verdict,
          rate_limit_verdict: .bridge.rate_limit_verdict,
          reply_event_id: .bridge.reply_event_id
     },
     source: .source,
     when: {
       matrix_origin_server_ts: .bridge.matrix_origin_server_ts,
       bridge_completed_at: .bridge.time,
       task_completed_at: .kagent.task.status.timestamp
     },
     usage: .kagent.task.metadata.kagent_usage_metadata,
     gateway_corroboration: .agentgateway.requests
   }' audit-evidence.json
   ```

For a completed kagent Task, `kagent_usage_metadata` contains `promptTokenCount`, `candidatesTokenCount`, and `totalTokenCount`. A terminal A2A Message can have a context but no task ID or task usage row; in that case the bundle reports a null task and per-task usage is not attributable. A blank `reply_event_id` means the agent operation ended but the bridge could not prove that its Matrix reply was delivered.

The collector selects exactly one terminal `accepted` record as the canonical durable invocation and retains every matching legacy record in `bridge_delivery_audits`. For an attempted delegation it also requires a valid `agent_version`, resolves that version against the live `agents.yaml`, and records the bridge and kagent Flux `lastAppliedRevision` values under `source`. A missing, malformed, unknown, or mismatched version invalidates the evidence bundle. A still-supported legacy local mapping may report an empty `agent_contract_sha256`; the bundle then proves the exact mapping and aligned Git revision but not a content digest of the effective Agent CRD/prompts. Repository-owned mappings must carry the pin before an operator claims full Agent-contract attribution. Collect promptly: if a later reconcile has already changed the live mapping, check out the recorded Git revision and reproduce the historical render instead of relabeling the old version as current.

An exact durable transaction replay does not produce a second terminal audit; its replay evidence is the transaction ID/body hash in Postgres. A legacy `duplicate` or `check_error` record remains visible without being mistaken for a second delegation. The collector fails closed if the canonical bridge record is absent or non-unique, a kagent API reports an error, or the task ID is not unique. An `outcome=ambiguous` bundle is evidence of uncertainty, not a resolved attribution; investigate it by deterministic A2A message ID. It emits session/task metadata only—not kagent event content, prompts, artifacts, authorization headers, or model outputs. The gateway section contains every candidate LLM/A2A request in the selected window and is labeled non-deterministic.

## Manual queries

The collector is the canonical path. These queries show where each fact originates and help diagnose a failed join.

### Matrix event

Use a read-scoped Matrix access token and avoid writing it to shell history:

```bash
ROOM_URI="$(jq -rn --arg value "${ROOM_ID}" '$value | @uri')"
EVENT_URI="$(jq -rn --arg value "${EVENT_ID}" '$value | @uri')"

xh -b GET \
  "https://matrix.${SERVER_NAME}/_matrix/client/v3/rooms/${ROOM_URI}/event/${EVENT_URI}" \
  Authorization:"Bearer ${MATRIX_ACCESS_TOKEN}" \
  | jq '{event_id, sender, room_id, origin_server_ts, type}'
```

This query deliberately omits `.content`. Inspect content only when the review requires the exact requested action and the reviewer is authorized for that room.

### Durable bridge ledger

As an authorized bridge-database operator, query the job by Matrix event ID with a content-free projection. Do not select `prompt`, `payload`, or `result_text`: active rows require those fields for recovery and they can contain room or agent content.

```sql
SELECT
  d.job_id,
  d.appservice_transaction_id,
  encode(t.body_sha256, 'hex') AS transaction_body_sha256,
  d.ghost_mxid,
  d.state,
  d.lease_generation,
  d.attempt_count,
  d.error_code,
  d.a2a_message_id,
  d.a2a_task_id,
  d.a2a_context_id,
  d.matrix_reply_event_id,
  d.matrix_placeholder_event_id,
  d.matrix_edit_event_id,
  d.created_at,
  d.updated_at,
  d.terminal_at
FROM bridge_delegations AS d
JOIN bridge_appservice_transactions AS t
  ON t.transaction_id = d.appservice_transaction_id
WHERE d.matrix_event_id = :'event_id';
```

Interpret the state before joining downstream evidence:

1. `pending` has no initial A2A attempt; an expired lease can be reclaimed.
1. `a2a_prepared` has a persisted deterministic message ID. A recorded pre-transport failure can retry, but recovery after a possibly started transport becomes `ambiguous` without resend.
1. `awaiting_task` has an acknowledged `a2a_task_id`; recovery calls `GetTask` and must not issue another initial `SendMessage`.
1. `reply_pending` has a durable result or notice awaiting Matrix projection; the stable reply/placeholder/edit transaction IDs make that projection idempotent per stage.
1. `delivered` and `denied` are content-scrubbed terminal tombstones retained for at least 24 hours before periodic cleanup. A capacity-denied row has `admission_checked=true`, `admission_allowed=false`, and stable `admission_reason`/`error_code` `queue_room_capacity_rejected` or `queue_global_capacity_rejected`; it never entered the claim queue. `ambiguous` and `dead` are content-scrubbed terminal evidence retained indefinitely for operator review.

Lease owner and generation are coordination evidence, not workload identity. A higher generation proves takeover of an expired/superseded claim; it does not by itself prove which protocol boundary the prior process reached. Use state, IDs, stable error code, terminal audit, and downstream evidence together.

### Appservice transaction conflict record

A changed-body reuse of an already-committed appservice transaction ID is rejected with HTTP 409 before the changed body reaches the mautrix consumer or creates durable work. The bridge emits exactly one content-free warning on its audit logger. Select it by transaction ID, not Matrix event ID:

```bash
kubectl -n bridge logs deploy/matrix-a2a-bridge --since=15m \
  | jq -R --arg transaction_id "${TRANSACTION_ID}" \
	      'fromjson? | select(.log_stream == "audit" and .msg == "appservice transaction conflict" and .transaction_id == $transaction_id)'
```

Require `audit_schema=fgentic.appservice_transaction.v1`, `outcome=rejected`, `terminal_reason=transaction_hash_conflict`, and distinct 64-character lowercase hexadecimal `stored_body_sha256` and `received_body_sha256` values. The record contains no request body, event content, prompt, or error string. The stored hash joins to the already-accepted `bridge_appservice_transactions` row; the rejected received hash exists only in the process-emitted record, so missing pod logs do not prove that no conflict occurred. Hash inequality proves a byte mismatch, not malicious intent: a homeserver, proxy, or operator defect can produce the same signal, and no downstream delegation/model evidence should exist for the rejected body.

### Bridge audit record

```bash
kubectl -n bridge logs deploy/matrix-a2a-bridge --since=15m \
  | jq -R --arg event_id "${EVENT_ID}" \
	      'fromjson? | select(.log_stream == "audit" and .msg == "delegation audit" and .matrix_event_id == $event_id)'
```

Interpret the terminal fields as follows:

| Field                                   | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| --------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sender_origin_kind`                    | `matrix` for ordinary local/federated Matrix senders or `bridge` when the full MXID matches one configured external-appservice namespace.                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `sender_origin_network`                 | Bounded configured network name such as `matrix`, `slack`, or `telegram`; it identifies the mapping authority, not a remote tenant or natural person.                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `agent_path`                            | Local kagent API path or configured remote A2A URL selected for this ghost. Treat a remote URL as target attribution only; Signed AgentCard verification is represented by the terminal stage/reason, not by the string itself.                                                                                                                                                                                                                                                                                                                                                                     |
| `agent_version`                         | `sha256:<64 lowercase hex>` over canonical JSON containing the mapping schema version, ghost, effective Agent-contract digest, route/trust/budget/stage/classification fingerprint, description, avatar, sorted sender/server policy, and normalized bridged-origin namespaces. It identifies the exact immutable mapping and origin snapshot captured at dispatch; it contains no room content, credentials, or model output.                                                                                                                                                                      |
| `agent_contract_sha256`                 | For a repository-owned local target, the SHA-256 of the effective kagent Agent spec plus every imported prompt fragment, generated by `cmd/agent-contract` and pinned in `agents.yaml`. Configured remote targets leave it empty because their complete route and Signed AgentCard trust pin are already included in `agent_version`; their runtime is outside this repository's version authority.                                                                                                                                                                                                 |
| `outcome`                               | `ok`, `failed`, `error`, `denied`, `rate_limited`, `queue_full`, `timeout`, `ambiguous`, or `dead` on the durable path. `queue_full` is the content-free signal for a terminal durable capacity denial, not an unrecorded drop. `ambiguous` means `SendMessage` may have reached the target but no acknowledgement was available, so the bridge did not resend. `dead` means bounded consecutive recovery failures were exhausted or persisted evidence was unusable. Legacy in-memory records may additionally carry `shutdown`, `lost`, `canceled`, or `deduplicated`.                            |
| `terminal_stage`                        | `queue`, `agent_card`, `matrix_register`, `matrix_join`, `admission`, `media_admission`, `message_send`, `message_result`, `task_poll`/`task_result`, or `recovery` on the durable path; this locates the last completed boundary. `queue` is a pre-limiter durable capacity denial. `message_send` plus `a2a_ack_ambiguous` is the conservative lost-ack terminal. `recovery` means the bounded failure budget was exhausted. Legacy-only records can additionally use `dedup`, `task_input`, or `task_cancel`. The independent membership-invite handler is outside this audit path.              |
| `terminal_reason`                       | Stable terminal explanation such as `queue_room_capacity_rejected`, `queue_global_capacity_rejected`, `completed`, `sender_policy_rejected`, `stage_policy_rejected`, `quote_over_budget`, `agent_mapping_changed`, `agent_card_untrusted`, `rate_limit_rejected`, `a2a_ack_ambiguous`, `agent_failed`, `request_timeout`, `task_timeout`, `task_poll_failed`, `input_required`, `auth_required_not_forwarded`, `empty_reply`, `media_input_rejected`, `matrix_delivery_failed`, or the last recovery error code. Legacy-only records may use shutdown, paused-input, or room-cancellation reasons. |
| `duration_ms`                           | Non-negative wall-clock time from durable job creation to terminal logging, including backlog wait, retries, and scheduled task polls. Legacy records retain their handler-specific duration semantics.                                                                                                                                                                                                                                                                                                                                                                                             |
| `dedup_verdict`                         | Durable terminal records use `accepted`; exact transaction replay is represented by `bridge_appservice_transactions`, while changed-body reuse is represented by `fgentic.appservice_transaction.v1`, not this legacy field. `duplicate` and `check_error` describe only the legacy processed-event path.                                                                                                                                                                                                                                                                                           |
| `rate_limit_verdict`                    | `allowed`, `rejected`, or `not_checked`. For a generated Matrix response this describes the independent notice plane, not invocation capacity. A durable admission denial before limiter evaluation uses `not_checked`; a persisted allowed/rejected decision is immutable across recovery.                                                                                                                                                                                                                                                                                                         |
| `a2a_attempted`                         | The durable path persists `true` with `a2a_prepared` before invoking the A2A client. `false` proves that boundary was not crossed; `true` means transport may have started, not that the target received or executed the request. Join downstream evidence only when context/task IDs or target-side records exist.                                                                                                                                                                                                                                                                                 |
| `a2a_user_id`                           | The value the bridge attached as `X-User-Id`; on attempted delegations it must equal `sender_mxid`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `canceled_by`                           | Full MXID of the room member who canceled a task on the legacy in-memory path; empty on durable job records because room-reaction cancellation is not yet persisted.                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `media_in`/`media_out`/`media_rejected` | Content-free file counts for one delegation (#115): files forwarded from the room to the agent, agent artifact files posted into the room, and files withheld in either direction by the media policy. Never a file name, byte, or MIME type — Matrix remains the record of content.                                                                                                                                                                                                                                                                                                                |
| `time`                                  | The bridge completion/log time; `matrix_origin_server_ts` is the request time reported by the event's origin homeserver.                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |

For a pre-admission remote-card refusal, require the complete tuple `outcome=denied`, `terminal_stage=agent_card`, `terminal_reason=agent_card_untrusted`, `rate_limit_verdict=not_checked`, and `a2a_attempted=false`. Do not join that record to kagent, a remote task, or model usage. If verified trust changes after the durable `a2a_prepared`/attempt boundary but the client guard refuses before HTTP, `rate_limit_verdict=allowed` and `a2a_attempted=true` record conservative client-invocation intent, not a network side effect; do not join downstream evidence. If trust changes while polling an already-started remote task, the same values accompany persisted context/task IDs; join only the earlier request/task evidence and never infer that the refused poll crossed transport. The provider-free integration fixture exercises the pre-admission case: it compares the remote stub's request counter before and after the rejected mention, then selects exactly one matching content-free audit from the bridge's JSON logs.

For a durable capacity refusal, require `outcome=queue_full`, `terminal_stage=queue`, one of the two stable capacity reasons, `rate_limit_verdict=not_checked`, and `a2a_attempted=false`; require one increment of `fgentic_delegations_total{outcome="queue_full"}` and no downstream A2A/model evidence. The atomic ledger layer persists the denial row and returns a content-free denial list. After a new transaction commits, the bridge consumes that list once to emit the metric and terminal audit, then may emit one best-effort catalog notice after transaction consumption through a fixed-size handoff and the shared notice buckets; exact replay emits none of them again. The ledger row remains authoritative because a process exit between database commit and process-local metric, log, or notice emission can lose the signal, so absence of a room notice is not evidence that admission succeeded.

### Agent version and rollback contract

The authoring gate computes `agentContractSHA256` from the effective kagent Agent CRD and imported prompt ConfigMap fragments. At config load, the bridge combines that digest with the complete normalized `agents.yaml` entry to derive `agent_version`, and binds the same contract digest into the local target fingerprint. Each immutable `AgentRef` therefore carries one version. The durable dispatcher copies it into the recovery payload before the persisted `a2a_prepared` transition and before calling A2A; terminal audit uses that copy even if `agents.yaml` reloads while the task is running. A prepared retry is refused before another A2A call when only the contract changed, because accepting a pre-contract fingerprint would let old evidence name a newly reconciled Agent. Non-durable refusal paths use the same immutable dispatch snapshot.

Rollback is ordinary GitOps, not a runtime registry operation:

1. Revert both the Agent CRD/prompt change and its matching `agentContractSHA256`/mapping change in Git.
1. Let Flux reconcile `kagent` and `bridge`; do not apply either resource manually.
1. Confirm both Kustomizations are Ready at their current generation and report the same intended `lastAppliedRevision`, then collect a new single-target audit probe. The collector rejects a mixed-revision reconcile window so a new bridge mapping cannot be attributed to an older kagent contract.
1. Require the new `agent_version` to equal the identifier produced before the bad change. Hash equality is the rollback proof; a Git commit message or pod age is not.

A mapping-only rollback is projected through the ConfigMap volume and adopted by the bridge's periodic fail-closed reload without a pod restart. A CRD or prompt rollback still requires kagent reconciliation, but not a bridge restart; the paired mapping digest must change in the same Git revision. An invalid mapping or stale contract is rejected by the authoring gates, and a failed runtime reload retains the last-known valid version rather than partially adopting it.

### kagent session and task

Take `SENDER`, `CONTEXT_ID`, and `TASK_ID` only from the bridge audit record. URL-encode the MXID before putting it in a query string.

```bash
SENDER_URI="$(jq -rn --arg value "${SENDER}" '$value | @uri')"

kubectl get --raw \
  "/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/sessions/${CONTEXT_ID}?user_id=${SENDER_URI}&limit=-1&order=asc" \
  | jq '.data | {session: .session | {id, user_id, agent_id, created_at, updated_at}, event_count: (.events | length)}'

kubectl get --raw \
  "/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/sessions/${CONTEXT_ID}/tasks?user_id=${SENDER_URI}" \
  | jq --arg task_id "${TASK_ID}" '
      .data[]
      | select(.id == $task_id)
      | {
          id,
          contextId,
          state: .status.state,
          completed_at: .status.timestamp,
          user_id: .metadata.kagent_user_id,
          session_id: .metadata.kagent_session_id,
          invocation_id: .metadata.kagent_invocation_id,
          usage: .metadata.kagent_usage_metadata
        }'
```

Do not print `.data.events`: those rows can contain prompts, outputs, errors, and a captured header map.

### agentgateway request logs

```bash
kubectl -n agentgateway-system logs deploy/agentgateway-proxy --since=15m \
  | jq -R '
      fromjson?
      | select(.scope == "request" and .protocol == "llm")
      | {
          time,
          route,
          status: .["http.status"],
          provider: .["gen_ai.provider.name"],
          request_model: .["gen_ai.request.model"],
          response_model: .["gen_ai.response.model"],
          input_tokens: .["gen_ai.usage.input_tokens"],
          output_tokens: .["gen_ai.usage.output_tokens"],
          duration
        }'
```

Match the task completion window, provider/model, and token counts. Never call that match unique when requests overlap: the current log has no Matrix event ID, MXID, A2A context/task ID, or kagent invocation ID. Agentgateway documents the default [LLM token metrics and request logs](https://agentgateway.dev/docs/kubernetes/latest/llm/observability/); its optional per-user accounting requires gateway authentication that this kagent model path does not currently use.

### PostgreSQL DDL/ROLE records

CloudNativePG creates the pgAudit extension in every connectable database and converts its CSV records into JSON stdout. Query the current primary from a repository checkout so the committed minimal projection removes statement and parameter fields even if configuration drifts:

```bash
PRIMARY="$(kubectl -n postgres get cluster platform-pg \
  -o jsonpath='{.status.currentPrimary}')"
kubectl -n postgres logs "pod/${PRIMARY}" --container postgres --since=15m \
  | jq -c -f scripts/lib/postgres-audit.jq
```

The selected records expose the database/session role, database, session ID, `DDL`/`ROLE` class, command, and object metadata. That metadata remains operationally sensitive and requires restricted access. ROLE records commonly have no object name, so the target role cannot be inferred from `object_name`. The raw CNPG record must hold `<not logged>` in both `.record.audit.statement` and `.record.audit.parameter`; normal `READ`/`WRITE` traffic is absent by configuration. `mise run check:postgres-audit` checks that contract offline, while `mise run test:postgres-audit` creates its own disposable kind cluster and proves it against the digest-pinned operand without touching the shared cluster.

### Kubernetes control-plane changes on local k3d

The local reference writes selected Kubernetes API events to `/var/log/kubernetes/audit/audit.log` inside `k3d-fgentic-server-0`. This query answers which API principal changed the bridge's governed route map or a bridge Secret without copying either object's body into the audit trail:

```bash
docker exec k3d-fgentic-server-0 sh -c \
  'cat /var/log/kubernetes/audit/audit.log /var/log/kubernetes/audit/audit-*.log 2>/dev/null' \
  | jq -c '
      select(
        .stage == "ResponseComplete" and
        (.responseStatus.code // 0) >= 200 and
        (.responseStatus.code // 0) < 300 and
        (.verb == "create" or .verb == "update" or .verb == "patch" or .verb == "delete") and
        .objectRef.namespace == "bridge" and
        (
          (.objectRef.resource == "configmaps" and
            .objectRef.name == "matrix-a2a-bridge-agents") or
          .objectRef.resource == "secrets"
        )
      )
      | {
          time: .requestReceivedTimestamp,
          principal: .user.username,
          groups: .user.groups,
          verb,
          resource: .objectRef.resource,
          name: .objectRef.name,
          response_code: .responseStatus.code,
          source_ips: .sourceIPs
        }'
```

The agent mapping is the `bridge/matrix-a2a-bridge-agents` **ConfigMap**, not a Secret. Registration, database, and API credentials remain namespace-local Secrets. A direct local `kubectl` call records its client-certificate principal; a Flux reconciliation records `system:serviceaccount:flux-system:kustomize-controller`. The latter proves which in-cluster controller applied the object, not which person authored the Git change—use the reviewed commit history for human authorship and the API record for reconciliation time/outcome. The successful-2xx filter means the result describes applied changes rather than rejected attempts. Metadata logging proves the verb, target, API identity, source, and response status; it intentionally cannot reconstruct a Secret/ConfigMap body or object diff.

The policy also records one-object `get` reads and writes for NetworkPolicies, kagent Agents, HelmReleases, Flux Kustomizations, ValidatingAdmissionPolicies/bindings, plus `pods/exec`. It drops list/watch and all unmatched traffic. Kubernetes `Metadata` is **body-suppressed, not content-free**: the event retains `requestURI`, and a `pods/exec` URI can include every `command=` argument. Never pass a credential or other sensitive value in exec arguments; treat access to this log as privileged and require #157 to preserve that boundary. `mise run check:kubernetes-audit` verifies the allowlist, canonical rotation flags, and disabled local kube-router controller; `mise run test:kubernetes-audit` creates an isolated no-port k3d cluster, proves an actual rotated sibling under a 1 MiB test override, churns NetworkPolicy objects without kube-router reconciliation failures, patches and reads a Secret, and proves that the Secret sentinel and request/response bodies are absent. It never touches the shared `fgentic` cluster.

### Prometheus aggregates

In one terminal, run:

```bash
kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090
```

Then query in another terminal:

```bash
xh -b GET http://localhost:9090/api/v1/query \
  query=='sum by (ghost, outcome) (fgentic_delegations_total)'

xh -b GET http://localhost:9090/api/v1/query \
  query=='sum by (from_state, to_state) (fgentic_delegation_ledger_transitions_total)'

xh -b GET http://localhost:9090/api/v1/query \
  query=='sum by (outcome) (fgentic_delegation_recovery_outcomes_total)'

xh -b GET http://localhost:9090/api/v1/query \
  query=='sum by (gen_ai_system, gen_ai_request_model, gen_ai_response_model, gen_ai_token_type) (agentgateway_gen_ai_client_token_usage_sum)'
```

These series corroborate that the agent, recovery, and model paths are active. They cannot select one event or user. Transition/recovery counters are process-emitted operational signals and can miss a database commit followed by an immediate process failure; the Postgres ledger remains the workflow source of truth. Do not add MXID, room, context, task, or event IDs as Prometheus labels; use the structured audit record for those high-cardinality joins.

## Retention and access controls

Retention is part of the evidence claim. A query returning nothing after its retention window is not proof that the invocation never happened.

| Store                        | Current repository posture                                                                                                                                                                                                                                                                                                                                                                                                                      | Retention knob / required control                                                                                                                                                                                                                                                                                   |
| ---------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Matrix events                | The production-shaped `local` and `gcp` profiles compose a finite Synapse policy: ordinary non-state events default to 90 days within a 1–365 day room-policy range, with a daily purge job. Demo and federation remain policy-free. State events, the last hidden message, redactions, media, federated copies, and client caches have separate semantics.                                                                                     | Change only the per-cluster `matrix_*_retention` platform settings with privacy/legal approval. The [retention and erasure runbook](retention.md) records the effective values, exceptions, edit/export ordering, and live evidence requirement.                                                                    |
| Bridge durable ledger        | Active jobs retain the recoverable Matrix event/prompt and later a pending result/notice. The terminal transition clears those content columns. Non-content `delivered`/`denied` tombstones and unreferenced transaction hashes become eligible for periodic cleanup after at least 24 hours; `ambiguous`/`dead` evidence remains indefinitely. Legacy `bridge_processed_events` markers retain their own minimum 24-hour compatibility window. | Restrict the bridge database and its backups as content-bearing while any job is active. Treat retained event/room/sender/A2A/Matrix IDs as linkable operational data. Do not delete ambiguous/dead evidence before investigation; changing these code-level windows requires privacy, recovery, and replay review. |
| Bridge audit logs            | A dedicated `slog` child logger emits `fgentic.appservice_transaction.v1` transaction conflicts, terminal `fgentic.delegation.v1` records, and `fgentic.delegation_ledger.v1` transitions to JSON stdout; application diagnostics have no such marker. Kubernetes/pod log availability is node-runtime dependent and is not a durable compliance archive.                                                                                       | Ship only reviewed fields from those schemas to an access-controlled immutable sink before claiming durable log retention. Never enable prompt/body capture in that pipeline, and do not substitute log survival for the Postgres recovery ledger.                                                                  |
| kagent sessions and tasks    | Persistent Postgres rows; Fgentic config declares no session/task TTL, and the supported per-user purge in #100 is not merged. Deletion or database loss is therefore the only current expiry.                                                                                                                                                                                                                                                  | Do not perform ad-hoc SQL erasure. Complete and validate #100 before claiming per-user purge. CloudNativePG backup catalog retention is `30d`, with a GCS 60-day lifecycle backstop, so deleted data may remain in protected backups until expiry.                                                                  |
| Kubernetes API (local k3d)   | Selected request/response-body-suppressed Metadata events exist only in `/var/log/kubernetes/audit/` inside each k3d server node. Request URIs remain sensitive; list/watch and unmatched resources are absent. Cluster deletion removes the trail.                                                                                                                                                                                             | Rotation starts at 10 MiB, retains at most three backups, and expires old backups after seven days. Issue #157 may collect this path only through its restricted, authenticated log pipeline; no durable retention is claimed today.                                                                                |
| PostgreSQL DDL/ROLE audit    | CNPG emits structured pgAudit records to pod stdout only. `pgaudit.log=ddl, role`; catalog-only traffic, SQL text, parameters, and normal reads/writes are disabled. This is best-effort logging, not transactional proof.                                                                                                                                                                                                                      | Kubernetes/node log rotation determines current availability. Issue #157 must select only the minimal SQL/payload-suppressed projection into its restricted queryable store before durable retention can be claimed.                                                                                                |
| agentgateway request logs    | JSON stdout only; `infra/agentgateway/parameters.yaml` configures format/level but no durable logging database or export.                                                                                                                                                                                                                                                                                                                       | Configure an approved log export and its own retention. Keep prompt/completion logging disabled unless separately authorized.                                                                                                                                                                                       |
| Prometheus                   | `7d` in `infra/observability/helmrelease.yaml`.                                                                                                                                                                                                                                                                                                                                                                                                 | Change `prometheus.prometheusSpec.retention`; size storage and privacy impact before increasing it.                                                                                                                                                                                                                 |
| Evidence bundle from script  | Caller-managed JSON containing MXID and room/event identifiers but no message content.                                                                                                                                                                                                                                                                                                                                                          | Store it only in the review case, restrict access, set case-specific retention, and delete it through the case process. Do not commit it.                                                                                                                                                                           |
| External model/provider logs | Provider-controlled and outside this runbook. The provider receives model traffic but no Matrix identity from the current path.                                                                                                                                                                                                                                                                                                                 | Apply the selected provider's retention/residency contract. It is not a substitute for the local evidence chain.                                                                                                                                                                                                    |

MXIDs, room IDs, event IDs, and task IDs are personal or linkable operational data. Restrict bridge, kagent, gateway, Prometheus, and Kubernetes log access accordingly. The evidence collector requires read access across namespaces; it is an operator tool, not an end-user endpoint.

The optional VictoriaLogs sink remains deliberately deferred rather than appearing as a dormant `HelmRelease`: this repository has no per-overlay log-storage opt-in that renders zero resources by default, and a suspended release would still add platform footprint. A future opt-in component must source-verify and immutably pin its chart and images; define namespace isolation, NetworkPolicy, disabled service-account token mounting, resource limits, PVC/backup posture, retention, authentication, and TLS; and select only the bridge/MCP audit markers, the local request/response-body-suppressed Kubernetes API stream, plus the minimal pgAudit projection above. Until that component and a live retention/access test exist, the reference platform makes no durable-log claim and the bridge has no runtime dependency on a log backend.

## What is not attributable

1. **A real person is not proven.** For a local event, Synapse proves that a valid Matrix session acted as an MXID. Shared accounts, stolen tokens, compromised clients, and account recovery are outside this chain. For a federated MXID, the remote homeserver vouches only for its own sender identifier.
1. **`X-User-Id` is not kagent authentication.** The bridge derives it from the appservice-delivered event, but kagent's unsecure mode accepts a caller-supplied header and otherwise falls back to a default user. An in-cluster caller that bypasses the bridge can spoof it. NetworkPolicy limits who can reach that endpoint; it does not turn the header into a credential. The same holds for the ActivityPub gateway: its `X-User-Id` and `X-Origin-*` headers are asserted attribution derived from the verified inbound actor, not a downstream credential.
1. **An ActivityPub actor URI is proven only as far as the border verified it.** The gateway's `a2a_user_id` is trustworthy because the F3/F4 border checked the HTTP Signature, allowlist, and object-integrity proof — not because kagent re-authenticated it. The remote instance vouches only for its own actor; a real person behind that actor is not proven, exactly as for a federated MXID. `origin.network` is the signing domain, never a re-authenticated tenant.
1. **The provenance prompt is not audit evidence.** The bridge also gives the model a provenance envelope for context. It is model input beside untrusted room text, can be imitated by prompt content, and must never replace the structured record.
1. **The gateway does not know the Matrix identity.** kagent does not forward `X-User-Id`, event ID, context ID, task ID, or invocation ID on its LLM request. Gateway time/model/token fields are therefore candidate corroboration, not a deterministic join—especially under concurrency or retries.
1. **Prometheus does not know the invocation.** Its labels are intentionally aggregate. A dashboard increase cannot prove which person or event caused it.
1. **Currency cost is not currently evidence-backed per task.** kagent records exact task tokens. Agentgateway can compute realized currency cost when a versioned cost catalog is configured, but this repository currently has no such catalog and no task correlation on the model request. Provider invoices aggregate independently. Report tokens and state currency cost as unavailable; do not multiply by an unversioned price copied from a website.
1. **Requested semantics disappear if Matrix content disappears.** The audit record proves event, room, and target, not the prompt body. If the Matrix event is redacted/purged and no authorized case capture exists, a reviewer can still prove that an agent was invoked but not reconstruct the requested instruction.
1. **An exact-body hash is not the request body.** The appservice transaction hash proves that a replay matched the bytes originally accepted under that transaction ID; the conflict record's stored/received pair proves only that the bytes differed. Without an authorized copy of those bytes, neither hash can reconstruct or explain the instruction. Hashes remain content-derived and can permit offline guessing of predictable bodies, so restrict them as linkable operational evidence even though raw content is absent.
1. **Ambiguous is not success or failure.** It proves only that A2A transport may have started and no acknowledgement was available. The bridge deliberately did not resend. Seek target-side evidence by deterministic message ID before deciding whether work executed, and preserve the tombstone if that evidence is unavailable.
1. **Successful work is distinct from a delivered reply.** The kagent task can complete even when Matrix projection remains `reply_pending` or exhausts recovery into `dead`. Require terminal ledger state `delivered`, a non-empty reply or edit event ID, and the corresponding Matrix event when delivery is material.
1. **Recovery fixtures are not an RTO.** Unit/contract tests prove state-machine behavior, `test:availability` exercises graceful SIGTERM, and `test:crash-recovery` SIGKILLs the bridge at six persisted boundaries. Even a green local fixture is not node-loss evidence or a production recovery-time objective.
1. **pgAudit is not a transaction ledger or human identity.** It records a statement when PostgreSQL executes it, even if the transaction later rolls back, and the standard logging path can lose a record before durable export. The database role is a session principal, not proof of one workload or person; the trail is not trustworthy against a hostile superuser who can change settings or logging. ROLE records may omit the target object name, while row reads/writes are intentionally outside the selected class.

Any mismatch between bridge sender/header, context/session, task/user, or task ID invalidates the attribution claim. Preserve the raw records, stop the review, and investigate for bypass, stale logs, version drift, or tampering. Future distributed tracing can make the gateway join deterministic, but it must propagate a non-forgeable, privacy-reviewed correlation context rather than treating the current header as authentication.

## Local verification gate

Run the app tests before deploying the audited bridge:

```bash
mise run check:audit-attribution
mise run check:kubernetes-audit
mise run check:postgres-audit
mise run test
mise run test:crash-recovery
```

The permanent tests cover the content-free audit schemas, exact-body replay/conflict, fenced ledger transitions, consecutive retry/dead behavior, acknowledgement ambiguity without resend, known-task recovery, deterministic Matrix projection, terminal content scrubbing/retention, explicit rate-limit verdict, A2A header/context contract, collector joins, and absent/ambiguous records. The crash-recovery task adds six process-kill boundaries against real Postgres, Synapse, and A2A; retain its candidate-revision result, because the fixture is not part of a green unit suite by implication. After deployment, complete one fresh mention and run the collector above. That live walk—not local test evidence alone—is the acceptance proof for the installed versions.

See also the [retention and erasure runbook](retention.md), [security policy](../SECURITY.md), [threat model](security.md), [observability spec](observability.md), and [bridge state/flow spec](bridge.md).
