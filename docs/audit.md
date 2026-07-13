---
type: Runbook
title: Delegation Attribution Audit
description: Evidence chains for one Matrix delegation plus SQL-suppressed PostgreSQL schema and role changes.
---

# Delegation Attribution Audit

This runbook answers four questions for one Matrix delegation: **who** invoked an agent, **what** target was invoked, **when** it ran, and **what model usage** the task consumed. The join key starts with the Matrix event ID and ends with the kagent task ID.

The current platform can prove the Matrix identity asserted through the bridge and persisted by local kagent. For configured remote targets, it can additionally prove that a fetched AgentCard matched the pinned A2A v1.0 ES256 identity before dispatch; an untrusted-card refusal has no downstream task evidence because dispatch never occurred. A successful remote delegation stops at the bridge's A2A IDs in local evidence—the partner must supply its own task, model, and token-usage records, and the kagent collector below is therefore local-target-only. The platform cannot prove that the same human controlled the Matrix account, authenticate that identity again at kagent, or deterministically attach an agentgateway request to one kagent task. Exact per-task token usage is available from local kagent; exact currency cost is not available until a versioned agentgateway cost catalog and cross-hop correlation are configured. Those are evidence limits, not implied guarantees.

## Evidence chain

```text
Matrix event_id
  └─ sender + room + origin_server_ts
       └─ bridge audit record (fgentic.delegation.v1)
            ├─ sender origin kind/network + agent path
            ├─ X-User-Id assertion
            ├─ A2A contextId ── kagent session.user_id
            └─ A2A task ID ──── kagent task metadata + exact token usage
                                      ⇢ agentgateway request logs by time/model/tokens
                                      ⇢ Prometheus aggregate counters
```

| Hop                  | Evidence and join fields                                                                                                                                                                                                                     | What it proves                                                                                                                                                                                                                                                                                        |
| -------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Matrix / Synapse     | `event_id`, `sender`, `room_id`, `origin_server_ts`, event content                                                                                                                                                                           | The homeserver accepted an event from an authenticated Matrix session for that MXID. Matrix is the source of record for the requested content.                                                                                                                                                        |
| Bridge               | JSON `msg="delegation audit"`, `log_stream="audit"`, `audit_schema="fgentic.delegation.v1"` records: Matrix event/sender/room, bounded sender-origin kind/network, target, A2A IDs, outcome/reason, duration, dedup, and rate-limit verdicts | Which mapped agent the bridge selected, whether a configured external appservice namespace classified the sender, which value it asserted in `X-User-Id`, and the terminal result. Prompts and message bodies are intentionally absent. A redelivery adds a duplicate record, not another delegation. |
| kagent session store | Session `id`, `user_id`, `agent_id`; task `id`, `contextId`, state/timestamp; `kagent_user_id`, `kagent_invocation_id`, `kagent_usage_metadata`                                                                                              | kagent persisted the asserted user under the same context and task. Task usage metadata is the exact per-task token evidence emitted by the agent runtime.                                                                                                                                            |
| agentgateway logs    | Request time, route, provider, requested/response model, HTTP status, input/output tokens                                                                                                                                                    | A model request crossed the governed egress path. The current kagent model call does not propagate Matrix, context, task, or invocation identity, so time/model/token matching is corroboration only.                                                                                                 |
| Prometheus           | `fgentic_delegations_total{ghost,outcome}` and `agentgateway_gen_ai_client_token_usage_sum` with provider/model/token dimensions                                                                                                             | Aggregate delegation and model consumption. Identity, room, context, and task labels are deliberately excluded to avoid personal-data and cardinality hazards.                                                                                                                                        |
| PostgreSQL / pgAudit | CNPG JSON stdout with `logger=pgaudit`, `msg=record`, database/session role, and `record.audit` class, command, object type/name, and statement IDs                                                                                          | A `DDL` or `ROLE` operation reached PostgreSQL under one database session role. This is an independent database-change hop, not a join to the Matrix delegation; SQL text and parameters are explicitly suppressed.                                                                                   |

The bridge sends `X-User-Id: <sender_mxid>` on A2A AgentCard, `message/send`, and `tasks/get` requests. The in-process contract test `TestClientContract_MessageContextAttributionAndWireVersion` is the wire-level proof. kagent 0.9.11 reads this header in its [unsecure authenticator](https://github.com/kagent-dev/kagent/blob/v0.9.11/go/core/internal/httpserver/auth/authn.go), then persists it with the context/session. Its runtime explicitly marks that identity as unauthenticated. NetworkPolicy is therefore the compensating boundary described in the [threat model](security.md).

The PostgreSQL row is deliberately orthogonal to the delegation chain. It can corroborate a schema or privilege change performed by a service/database role, but it carries no Matrix event, task, workload, or human identity and must not be presented as delegation attribution.

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

Prerequisites are a reconciled cluster, the audited bridge build, `kubectl` access to the four namespaces, and `jq`. Keep the event ID in single quotes because Matrix event IDs begin with `$`.

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
          task_id: .bridge.a2a_task_id,
          outcome: .bridge.outcome,
          terminal_reason: .bridge.terminal_reason,
          duration_ms: .bridge.duration_ms,
          dedup_verdict: .bridge.dedup_verdict,
          rate_limit_verdict: .bridge.rate_limit_verdict,
          reply_event_id: .bridge.reply_event_id
     },
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

The collector selects exactly one `accepted` or `check_error` record as the canonical invocation and retains every matching record in `bridge_delivery_audits`. A later `duplicate` delivery is therefore visible evidence without being mistaken for a second delegation. The collector fails closed if the canonical bridge record is absent or ambiguous, a kagent API reports an error, or the task ID is not unique. It emits session/task metadata only—not kagent event content, prompts, artifacts, authorization headers, or model outputs. The gateway section contains every candidate LLM/A2A request in the selected window and is labeled non-deterministic.

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

### Bridge audit record

```bash
kubectl -n bridge logs deploy/matrix-a2a-bridge --since=15m \
  | jq -R --arg event_id "${EVENT_ID}" \
	      'fromjson? | select(.log_stream == "audit" and .msg == "delegation audit" and .matrix_event_id == $event_id)'
```

Interpret the terminal fields as follows:

| Field                   | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `sender_origin_kind`    | `matrix` for ordinary local/federated Matrix senders or `bridge` when the full MXID matches one configured external-appservice namespace.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `sender_origin_network` | Bounded configured network name such as `matrix`, `slack`, or `telegram`; it identifies the mapping authority, not a remote tenant or natural person.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `agent_path`            | Local kagent API path or configured remote A2A URL selected for this ghost. Treat a remote URL as target attribution only; Signed AgentCard verification is represented by the terminal stage/reason, not by the string itself.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `outcome`               | `ok`, `failed`, `error`, `denied`, `rate_limited`, `queue_full`, `shutdown`, `timeout`, `lost`, or `canceled`. `denied` includes a bridged-sender policy rejection, queued policy/mapping revocation, or untrusted remote AgentCard before A2A. `rate_limited` may also mean a generated notice was suppressed. `queue_full` means the bounded dispatcher rejected the target before rate admission or A2A. `shutdown` means a resolved target was rejected or dropped before dispatch while the process stopped. `canceled` means an authorized room member stopped a running long task by reacting to its placeholder. `deduplicated` is audit-only and means this delivery produced no dispatch or A2A request. |
| `terminal_stage`        | `dedup`, `queue`, `agent_card`, `matrix_register`, `matrix_join`, `admission`, `message_send`, `message_result`, `task_poll`/`task_result`, or `task_cancel`; this locates the last completed boundary. `agent_card` is the fail-closed remote trust check. It normally occurs before ghost registration and limiter admission, but may also record a generation change caught at the remote HTTP boundary or while polling an already-started task. `task_cancel` records a room-initiated cancel of an in-flight task. The independent membership-invite handler is outside this audit path.                                                                                                                     |
| `terminal_reason`       | Stable terminal explanation such as `completed`, `duplicate_delivery`, `queue_room_capacity_rejected`, `queue_global_capacity_rejected`, `shutdown_enqueue_rejected`, `shutdown_queued_dropped`, `sender_policy_rejected`, `stage_policy_rejected`, `quote_over_budget`, `agent_mapping_changed`, `agent_card_untrusted`, `rate_limit_rejected`, `denial_notice_rate_limit_rejected`, `a2a_call_failed`, `agent_failed`, `task_timeout`, `task_poll_failed`, or `canceled_by_room`.                                                                                                                                                                                                                                |
| `duration_ms`           | Non-negative wall-clock duration for bridge dispatch and terminal handling; for a duplicate it covers the dedup check only. Queue wait is excluded.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `dedup_verdict`         | `accepted` for a first delivery, `duplicate` for a suppressed redelivery, or `check_error` when the store failed and the bridge deliberately proceeded. `check_error` weakens the effectively-once guarantee and requires review.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `rate_limit_verdict`    | `allowed`, `rejected`, or `not_checked`. For a generated Matrix response this describes the independent notice plane, not invocation capacity. Queue-capacity/shutdown rejection, registration/join failures, queued mapping/policy revocations, and duplicate deliveries use `not_checked`.                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `a2a_attempted`         | `false` means no A2A request was attempted, so `a2a_user_id`, context, and task must not be treated as downstream evidence.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `a2a_user_id`           | The value the bridge attached as `X-User-Id`; on attempted delegations it must equal `sender_mxid`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `canceled_by`           | Full MXID of the room member who canceled a running long task (`outcome=canceled`); empty on every other record. It may differ from `sender_mxid`: a room moderator at `CANCEL_MODERATOR_POWER_LEVEL` can cancel another member's delegation.                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `time`                  | The bridge completion/log time; `matrix_origin_server_ts` is the request time reported by the event's origin homeserver.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |

For a pre-admission remote-card refusal, require the complete tuple `outcome=denied`, `terminal_stage=agent_card`, `terminal_reason=agent_card_untrusted`, `rate_limit_verdict=not_checked`, and `a2a_attempted=false`. Do not join that record to kagent, a remote task, or model usage. If verified trust changes after admission but before the initial HTTP handoff, the same refusal has `rate_limit_verdict=allowed` and still has `a2a_attempted=false`. If trust changes while polling an already-started remote task, it has `rate_limit_verdict=allowed` and `a2a_attempted=true`; join only the earlier request/task evidence and never infer that the refused poll crossed the transport boundary. The provider-free integration fixture exercises the pre-admission case: it compares the remote stub's request counter before and after the rejected mention, then selects exactly one matching content-free audit from the bridge's JSON logs.

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
  query=='sum by (gen_ai_system, gen_ai_request_model, gen_ai_response_model, gen_ai_token_type) (agentgateway_gen_ai_client_token_usage_sum)'
```

These series corroborate that the agent and model paths are active. They cannot select one event or user. Do not add MXID, room, context, task, or event IDs as Prometheus labels; use the structured audit record for those high-cardinality joins.

## Retention and access controls

Retention is part of the evidence claim. A query returning nothing after its retention window is not proof that the invocation never happened.

| Store                        | Current repository posture                                                                                                                                                                                                                                                                                                 | Retention knob / required control                                                                                                                                                                                                                                                     |
| ---------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Matrix events                | No Synapse message-retention policy is configured. Upstream Synapse disables message-retention enforcement by default, so ordinary room events are not age-purged by a server policy. Redaction and client caches have separate semantics.                                                                                 | Configure Synapse `retention` through `infra/matrix/helmrelease.yaml` only with privacy/legal approval. The [Synapse configuration manual](https://element-hq.github.io/synapse/latest/usage/configuration/config_documentation.html#retention) documents the default and purge jobs. |
| Bridge audit logs            | A dedicated `slog` child logger emits JSON stdout with `log_stream=audit`; application diagnostics have no such marker. Kubernetes/pod log availability is node-runtime dependent and is not a durable compliance archive. The 24-hour `bridge_processed_events` pruning window is deduplication state, not log retention. | Ship only records matching both `log_stream=audit` and `audit_schema=fgentic.delegation.v1` to an access-controlled immutable sink before claiming durable retention. Never enable prompt/body capture in that pipeline.                                                              |
| kagent sessions and tasks    | Persistent Postgres rows; Fgentic config declares no session/task TTL. Deletion or database loss is therefore the only current expiry.                                                                                                                                                                                     | Establish a kagent deletion policy before production. CloudNativePG backup catalog retention is `30d` in `infra/postgres/cluster.yaml`, so deleted data may remain in protected backups until expiry.                                                                                 |
| PostgreSQL DDL/ROLE audit    | CNPG emits structured pgAudit records to pod stdout only. `pgaudit.log=ddl, role`; catalog-only traffic, SQL text, parameters, and normal reads/writes are disabled. This is best-effort logging, not transactional proof.                                                                                                 | Kubernetes/node log rotation determines current availability. Issue #157 must select only the minimal SQL/payload-suppressed projection into its restricted queryable store before durable retention can be claimed.                                                                  |
| agentgateway request logs    | JSON stdout only; `infra/agentgateway/parameters.yaml` configures format/level but no durable logging database or export.                                                                                                                                                                                                  | Configure an approved log export and its own retention. Keep prompt/completion logging disabled unless separately authorized.                                                                                                                                                         |
| Prometheus                   | `7d` in `infra/observability/helmrelease.yaml`.                                                                                                                                                                                                                                                                            | Change `prometheus.prometheusSpec.retention`; size storage and privacy impact before increasing it.                                                                                                                                                                                   |
| Evidence bundle from script  | Caller-managed JSON containing MXID and room/event identifiers but no message content.                                                                                                                                                                                                                                     | Store it only in the review case, restrict access, set case-specific retention, and delete it through the case process. Do not commit it.                                                                                                                                             |
| External model/provider logs | Provider-controlled and outside this runbook. The provider receives model traffic but no Matrix identity from the current path.                                                                                                                                                                                            | Apply the selected provider's retention/residency contract. It is not a substitute for the local evidence chain.                                                                                                                                                                      |

MXIDs, room IDs, event IDs, and task IDs are personal or linkable operational data. Restrict bridge, kagent, gateway, Prometheus, and Kubernetes log access accordingly. The evidence collector requires read access across namespaces; it is an operator tool, not an end-user endpoint.

The optional VictoriaLogs sink remains deliberately deferred rather than appearing as a dormant `HelmRelease`: this repository has no per-overlay log-storage opt-in that renders zero resources by default, and a suspended release would still add platform footprint. A future opt-in component must source-verify and immutably pin its chart and images; define namespace isolation, NetworkPolicy, disabled service-account token mounting, resource limits, PVC/backup posture, retention, authentication, and TLS; and select only the bridge/MCP audit markers plus the minimal pgAudit projection above. Until that component and a live retention/access test exist, the reference platform makes no durable-log claim and the bridge has no runtime dependency on a log backend.

## What is not attributable

1. **A real person is not proven.** For a local event, Synapse proves that a valid Matrix session acted as an MXID. Shared accounts, stolen tokens, compromised clients, and account recovery are outside this chain. For a federated MXID, the remote homeserver vouches only for its own sender identifier.
1. **`X-User-Id` is not kagent authentication.** The bridge derives it from the appservice-delivered event, but kagent's unsecure mode accepts a caller-supplied header and otherwise falls back to a default user. An in-cluster caller that bypasses the bridge can spoof it. NetworkPolicy limits who can reach that endpoint; it does not turn the header into a credential. The same holds for the ActivityPub gateway: its `X-User-Id` and `X-Origin-*` headers are asserted attribution derived from the verified inbound actor, not a downstream credential.
1. **An ActivityPub actor URI is proven only as far as the border verified it.** The gateway's `a2a_user_id` is trustworthy because the F3/F4 border checked the HTTP Signature, allowlist, and object-integrity proof — not because kagent re-authenticated it. The remote instance vouches only for its own actor; a real person behind that actor is not proven, exactly as for a federated MXID. `origin.network` is the signing domain, never a re-authenticated tenant.
1. **The provenance prompt is not audit evidence.** The bridge also gives the model a provenance envelope for context. It is model input beside untrusted room text, can be imitated by prompt content, and must never replace the structured record.
1. **The gateway does not know the Matrix identity.** kagent does not forward `X-User-Id`, event ID, context ID, task ID, or invocation ID on its LLM request. Gateway time/model/token fields are therefore candidate corroboration, not a deterministic join—especially under concurrency or retries.
1. **Prometheus does not know the invocation.** Its labels are intentionally aggregate. A dashboard increase cannot prove which person or event caused it.
1. **Currency cost is not currently evidence-backed per task.** kagent records exact task tokens. Agentgateway can compute realized currency cost when a versioned cost catalog is configured, but this repository currently has no such catalog and no task correlation on the model request. Provider invoices aggregate independently. Report tokens and state currency cost as unavailable; do not multiply by an unversioned price copied from a website.
1. **Requested semantics disappear if Matrix content disappears.** The audit record proves event, room, and target, not the prompt body. If the Matrix event is redacted/purged and no authorized case capture exists, a reviewer can still prove that an agent was invoked but not reconstruct the requested instruction.
1. **Successful work is distinct from a delivered reply.** The kagent task can complete even when the Matrix reply fails. Require a non-empty `reply_event_id`, and query that Matrix event when reply delivery is material.
1. **pgAudit is not a transaction ledger or human identity.** It records a statement when PostgreSQL executes it, even if the transaction later rolls back, and the standard logging path can lose a record before durable export. The database role is a session principal, not proof of one workload or person; the trail is not trustworthy against a hostile superuser who can change settings or logging. ROLE records may omit the target object name, while row reads/writes are intentionally outside the selected class.

Any mismatch between bridge sender/header, context/session, task/user, or task ID invalidates the attribution claim. Preserve the raw records, stop the review, and investigate for bypass, stale logs, version drift, or tampering. Future distributed tracing can make the gateway join deterministic, but it must propagate a non-forgeable, privacy-reviewed correlation context rather than treating the current header as authentication.

## Local verification gate

Run the app tests before deploying the audited bridge:

```bash
mise run check:audit-attribution
mise run check:postgres-audit
mise run test
```

The permanent tests cover the content-free audit schema, explicit rate-limit verdict, replay suppression with duplicate evidence, A2A header/context contract, successful/redelivered collector joins, absent/ambiguous bridge records, and the valid empty-task-ID case. After deployment, complete one fresh mention and run the collector above. That live walk—not a green unit suite alone—is the acceptance proof for the installed versions.

See also the [security policy](../SECURITY.md), [threat model](security.md), [observability spec](observability.md), and [bridge state/flow spec](bridge.md).
