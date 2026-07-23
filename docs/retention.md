---
type: Runbook
title: Retention and Data-Subject Erasure
description: Finite Matrix retention, store-by-store limits, and the honest MAS-to-Synapse erasure workflow.
---

# Retention and data-subject erasure

This runbook answers five operator questions: what Fgentic keeps, where it is stored, how long the reference keeps it, how to process one user's erasure request, and what the platform cannot retract. It is a technical control pack, not a legal basis or a substitute for the deployment's privacy schedule.

## Policy boundary

The `local` and `gcp` overlays compose `infra/matrix/retention`; their durations come from the overlay's `platform-settings` ConfigMap. The base Matrix release, disposable `demo`, and federation lab do not compose that Component. This keeps evaluation behavior unchanged and prevents a test-lab policy from being mistaken for a bilateral production commitment.

The tracked `local` and `gcp` reference values are deliberately finite:

| Data / control                                 | Reference value | Effective behavior                                                                                                                                                                                          |
| ---------------------------------------------- | --------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Ordinary non-state room events                 | `90d` default   | Synapse stops serving an expired event immediately and removes it on the next purge job.                                                                                                                    |
| Room-policy lower / upper bound                | `1d` / `365d`   | A room's experimental `m.room.retention.max_lifetime` is clamped into this range when purge jobs evaluate it.                                                                                               |
| Purge job                                      | every `1d`      | One job without lifetime boundaries covers every policy range; database removal may lag expiry by up to its next run.                                                                                       |
| Locally uploaded / remote-cached media         | `180d` / `30d`  | Synapse evaluates media age from last access. Access refreshes the clock, so this is not a creation-date deletion guarantee.                                                                                |
| Original content behind a redacted event       | `7d`            | The redacted event remains part of room history; only the retained original content is eligible for removal.                                                                                                |
| Rooms forgotten by every local user            | `30d`           | Synapse can purge local room history only after every local user has forgotten the room.                                                                                                                    |
| CloudNativePG backup catalog / bucket backstop | `30d` / `60d`   | CNPG declares 30-day retention; the GCS lifecycle deletes objects at 60 days as a failure backstop. Purged application rows can therefore survive in protected backups until the applicable object expires. |
| Prometheus                                     | `7d`            | Operational metrics follow their own storage policy and contain aggregate, potentially linkable metadata.                                                                                                   |

These are reviewable reference values, not universally correct legal periods. Change them only with the room purposes, exports, holds, partner duties, storage capacity, and deletion evidence in the same review. `mise run check:retention-policy` proves that both production-shaped profiles render the declared values and that demo/federation remain policy-free.

### Synapse semantics that constrain the claim

The [Synapse retention guide](https://element-hq.github.io/synapse/latest/message_retention_policies.html) is the authority for the following limits:

1. The server default is stable, but per-room `m.room.retention` remains experimental. Synapse records `min_lifetime` but does not implement its semantics; this pack therefore relies on `allowed_lifetime_min`, which clamps the effective `max_lifetime` used by purge jobs.
1. State events are not covered. Synapse also retains at least one message per room, although it hides that expired message from clients.
1. Expiry and physical deletion are separate. An expired event is hidden immediately, then removed by a later purge job; PostgreSQL can reuse the freed space without returning it to the filesystem immediately.
1. Media retention is access-based and excludes quarantined or protected media under upstream rules. Account deactivation and event redaction do not themselves delete uploaded media.
1. Every participating homeserver applies its own policy. A local policy cannot force a federated peer, client cache, model provider, external bridge, export, or backup copy to delete.

## Bridge edits and export ordering

The bridge posts a placeholder and later sends the final answer as a new `m.room.message` event with an `m.replace` relation to that placeholder. Both events receive their own origin timestamp. An edit-aware client needs the original event to reconstruct the replacement.

The reference's one-day lower clamp exceeds the bridge's ten-minute `TASK_TIMEOUT`, so a normally processed placeholder cannot expire inside its task window. This protects the live placeholder-to-final-edit flow. It does **not** promise an edited view after the original placeholder expires: the relation target can be hidden or purged, and clients may render only fallback content or no reconstructed edit. Keep any room whose edited transcript must remain readable above the required evidence window, and test the actual approved client.

Export and legal hold must happen before expiry. A hidden or purged Matrix event cannot be reconstructed from the content-free bridge audit record or its transaction hash. The repository does not yet ship the compliance export from [#138](https://github.com/fmind-ai/fgentic/issues/138); use an approved external export process, verify its completeness, then record the export and hold owner before allowing the room policy to expire data. A database backup is recovery material, not a compliance export or legal-hold interface.

## Data-subject erasure runbook

Run this workflow under an approved request record. Keep identifiers and results in that restricted case; do not put personal data, access tokens, or evidence bundles in git, issue comments, command history, or general-purpose logs.

### 1. Scope and freeze

1. Resolve the data subject to the upstream IdP subject, MAS user ID/localpart, full Matrix ID, local rooms, federated rooms, uploaded media, kagent sessions/tasks, bridge ledger/audit identifiers, provider requests, exports, and backups.
1. Decide whether an active legal hold or another overriding duty applies. If export or preservation is required, complete and verify it **before** any purge or redaction.
1. Stop new work: remove the Matrix ID from applicable sender allowlists and room memberships, and identify any already queued delegation. Deactivation prevents new login; it does not prove every independent downstream task stopped.

### 2. Deactivate through MAS

ESS delegates Matrix authentication to MAS under MSC3861. Use the [MAS Admin API](https://element-hq.github.io/matrix-authentication-service/topics/admin-api.html) or its supported CLI; do not call Synapse's legacy deactivate/erase endpoint directly.

```bash
# Run in the MAS administrative environment after separately recording the resolved localpart.
mas-cli manage lock-user "${MATRIX_LOCALPART}" --deactivate
```

The [MAS retention contract](https://element-hq.github.io/matrix-authentication-service/topics/data-retention.html) says deactivation blocks login, finishes active sessions, revokes personal access tokens, removes attached email addresses and unsupported identifiers, and asks Synapse to deactivate the user. Finished MAS sessions and their linked claims can remain for 30 days. The MAS user row, password-history rows, and upstream SSO mappings have documented residuals.

Record the MAS user ID, operation time, operator identity, and verified deactivated status. Do not treat a successful HTTP/CLI exit alone as downstream Synapse evidence.

### 3. Redact the user's room events

MAS deactivation is not deletion of sent content. Use Synapse's [redact-all-user-events Admin API](https://element-hq.github.io/synapse/latest/admin_api/user_admin_api.html#redact-all-the-events-of-a-user) with a short-lived admin token held outside shell history. For a local user, allow Synapse to puppet that user; forcing `use_admin` can fail in rooms where the admin lacks sufficient power.

```bash
USER_URI="$(jq -rn --arg value "${MATRIX_USER_ID}" '$value | @uri')"

xh -b POST \
  "https://matrix.${SERVER_NAME}/_synapse/admin/v1/user/${USER_URI}/redact" \
  Authorization:"Bearer ${SYNAPSE_ADMIN_TOKEN}" \
  rooms:='[]' \
  limit:=100000 \
  | jq -er '.redact_id' > "${REDACT_ID_FILE}"
```

Choose `limit` from a pre-operation event inventory rather than copying the example blindly. Poll `GET /_synapse/admin/v1/user/redact_status/<redact_id>` until `completed`; fail the request if `failed_redactions` is non-empty. Re-inventory and repeat if the approved bound did not cover every event. Verify from a second account in every representative room, including a federated room when one is in scope.

Redaction removes event content from normal views but leaves event IDs, sender/timestamp relationships, room state, membership history, and redaction events. Synapse can keep original redacted content for `matrix_redaction_retention` before deleting it. The [upstream erasure limitation class](https://github.com/element-hq/synapse/issues/15355) and this runbook's honesty section remain applicable.

### 4. Delete user media separately

Inventory and delete the local user's uploads through Synapse's user-media Admin API, then verify the returned IDs and remaining inventory. Redacting a message does not delete its media object, and deleting media can break other events that reference the same URI. Remote caches and federated copies remain under their operators' policies.

### 5. Purge agent and integration state

1. For a local room/agent conversation, have an authorized invoker or room moderator run `!forget <agent>` (or `/forget <agent>` in a raw Matrix client). The bridge refuses remote mappings, active work, and legacy contexts whose complete owner set is unknown. Otherwise it deletes the kagent session for every recorded Matrix owner, requires each follow-up read to return `404`, and only then drops the bridge context so the next delegation starts fresh. Record the in-room result; a success already means the verified deletion/reset completed. Never substitute ad-hoc SQL.
1. A local agent mapping may set `maxSessionAge` to apply the same verified reset in bounded sweeps. Omission retains the conversation until explicit forget. A failed deletion keeps the bridge context, while an incomplete pre-governance owner inventory remains an operator-resolved residual rather than a claimed purge.
1. The bridge clears prompt/result content at terminal transition. Ordinary content-free terminal tombstones become cleanup-eligible after at least 24 hours, while `ambiguous` and `dead` evidence remains indefinitely for investigation. Review those rows by purpose and do not destroy recovery evidence through unsupported SQL.
1. Review enabled external bridges, MCP servers, model providers, exports, case systems, and downstream log sinks independently. This repository cannot delete their copies.

### 6. Let backups expire and close with evidence

Application deletion does not rewrite existing backups. The GCP reference's CNPG catalog declares `30d`; its bucket lifecycle is a 60-day hard backstop if normal catalog cleanup fails. Restrict restores during that interval, and reapply the approved redaction/purge after any restore that rehydrates the subject. Local deployments have no repository-configured object-store backup, which is not proof that a host, volume, or operator snapshot does not exist.

Close the request only with timestamps and results for MAS deactivation, Synapse redaction status, second-account verification, media inventory/deletion, kagent disposition, downstream processors, backup-expiry date, federation notices, and every accepted residual.

## What this cannot erase

1. State events, the final hidden message Synapse keeps per room, event metadata, membership history, and some account/audit identifiers are outside ordinary message purging.
1. Redaction is a replacement of visible content, not proof that every database row, backup, client cache, notification, search index, screenshot, export, or model/provider copy disappeared.
1. Room history already replicated to another homeserver is outside unilateral control. [Federation §8.1](federation.md#81-what-matrix-federation-gives--stated-honestly) requires contractual partner deletion duties because cross-server redaction is best effort.
1. Deactivated MAS session rows, SSO mappings, user records, and other upstream-documented residuals remain for their separate periods or purposes.
1. `!forget` and `maxSessionAge` are functional local-conversation resets, not physical erasure: kagent soft-deleted rows/events, incomplete legacy owner sets, remote-agent state, and independent agent memory/tool stores remain outside that control. CloudNativePG backups can retain pre-reset rows until expiry.
1. Content-free does not mean anonymous. Matrix IDs, room/event IDs, timestamps, A2A IDs, IP addresses, and request identifiers can still be personal or linkable data.

## Audit-record exemption

Chat retention does not govern operational or legal-basis evidence. The content-free `fgentic.delegation.v1`, transaction-conflict, and ledger-transition records established by [#37](https://github.com/fmind-ai/fgentic/issues/37) prove invocation and recovery state without retaining the prompt body. They follow the separate [audit retention and access controls](audit.md#retention-and-access-controls): the bridge database has code-level windows, stdout has no durable guarantee, Prometheus uses seven days, and a future log sink must declare its own approved period.

Never keep chat content longer merely to preserve an audit join. If the request semantics must remain provable after chat expiry, create an authorized case export before purge with its own access and retention decision.

## Acceptance evidence

Static policy evidence is `mise run check:retention-policy`, `mise run check:manifests`, and `mise run check:overlays`. A shared-runtime owner must additionally record a candidate-revision drill on a local production-shaped cluster:

1. Create a scratch room with a short allowed `m.room.retention.max_lifetime`, send an ordinary message and a bridge placeholder/final edit, and record their event IDs and origin timestamps.
1. Prove the original and replacement render during the task window; after expiry and a purge-job interval, prove the ordinary event is unavailable and record the documented last-message/state-event exceptions.
1. Create a second test user, send representative text and media, deactivate it through MAS, run redact-all, poll completion, and prove from another account that content is redacted; inventory and delete its media separately.
1. Record the exact ESS/Synapse/MAS revision, rendered policy, times, redaction failures, remaining identifiers, and backup/federation caveats. Synthetic data only.

This repository does not convert the offline render into that runtime result. Keep the issue open until the drill passes on the candidate revision.
