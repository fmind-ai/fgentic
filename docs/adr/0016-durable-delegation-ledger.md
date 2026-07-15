---
type: Architecture Decision Record
title: Durable Delegation Ledger with At-Most-Once A2A Recovery
description: Accept work before appservice acknowledgement, recover it through fenced Postgres leases, and never blindly resend an acknowledgement-ambiguous A2A call.
---

# 0016 — Durable Delegation Ledger with At-Most-Once A2A Recovery

Status: Accepted

Approval: [maintainer decision for issue #311](https://github.com/fmind-ai/fgentic/issues/311#issuecomment-4965774096)

## Context

D3 introduced a bounded in-memory dispatcher to keep A2A work asynchronous, cap active model calls, and order each room. D4 moved mautrix state, conversation contexts, and an event-level processed marker to Postgres. Those controls fixed duplicate redelivery during an ordinary restart, but their acceptance boundary was incomplete: mautrix could return HTTP 200 before the bridge handler created work, and the bridge wrote its processed-event marker before A2A and Matrix completion. A hard process failure could therefore lose an acknowledged delegation while suppressing its replay.

Holding the Synapse transaction open for the complete delegation would serialize long tasks and still would not resolve a lost A2A or Matrix response. Distributed exactly-once execution is also unavailable across Synapse, Postgres, Matrix, and independently operated A2A targets: a sender-generated A2A message ID is correlation, while the supported targets do not prove persistent message-ID deduplication.

The maintainer selected issue #311's fail-closed option: remove silent loss, preserve per-room order, and prefer an explicit uncertain outcome over a duplicate model charge or side effect. Recoverable content may live in the bridge database only until the job becomes terminal; ordinary non-content tombstones remain for a minimum 24-hour replay window.

## Decision

1. Wrap the appservice transaction route with authenticated, bounded intake. Hash the exact request bytes and atomically commit the transaction plus every eligible `(Matrix event, target ghost)` job before HTTP 200. An exact replay is idempotent; a changed body under the same transaction ID is HTTP 409; a storage/classification failure is non-2xx so Synapse can retry.
1. Replace initial-mention use of the process-local dispatcher with a versioned Postgres ledger. Checked workflow states, expiring owner/generation-fenced leases, one claim coordinator, and `CONCURRENCY` preserve global active-work bounds. `ROOM_QUEUE_CAPACITY` and `GLOBAL_QUEUE_CAPACITY` bound every non-terminal durable job, including delayed and leased work. Serialize count-and-insert across transactions; for each new target check room capacity before global capacity and include accepted targets earlier in the same batch. A refused target still commits before acknowledgement as a content-scrubbed terminal `denied` row with stable reason `queue_room_capacity_rejected` or `queue_global_capacity_rejected`. The oldest accepted non-terminal job blocks later accepted work only in its room, so per-room FIFO survives process replacement while unrelated rooms can progress.
1. Persist a deterministic A2A message ID and a content-free attempt boundary in `a2a_prepared` before invoking the client. Start the whole-task deadline at the first persisted attempt boundary rather than ledger admission, so durable room-backlog time does not become a false agent timeout. A failure proven to be pre-transport may retry with that identity. Once transport may have started, an unavailable acknowledgement becomes terminal `ambiguous` and is not resent. A known task ID resumes only through `GetTask`. Any future at-least-once mode requires a separate, explicitly enabled target idempotency contract and acceptance test.
1. Persist an agent result or generic notice as `reply_pending` before Matrix projection. Reply, placeholder, edit, and per-index artifact events use deterministic Matrix transaction IDs; the primary returned event ID is immutable ledger evidence. Commit a changed conversation context in the same database transaction as its job transition.
1. Retry recoverable storage, Matrix, preflight, and task-poll failures with capped exponential backoff. Count consecutive recovery failures, reset on a successful transition or healthy poll, and terminate exhausted work as `dead` rather than retrying forever.
1. Retain the original event/prompt and pending result/notice only until the terminal transition clears those fields. Keep non-content `delivered`/`denied` tombstones for at least 24 hours before periodic cleanup; retain `ambiguous`/`dead` evidence indefinitely. Honor the previous `bridge_processed_events` marker for its minimum 24-hour compatibility window.
1. Keep one ready appservice intake replica and the zero-surge rollout. Graceful shutdown stops new claims and drains active leases; after hard process failure a replacement reclaims expired work. This is durable work recovery, not uninterrupted intake availability, distributed exactly-once execution, or a process-recovery RTO.
1. Do not claim durable typing, paused input-required continuation, room-reaction cancellation, intermediate progress posts, or pin state. The production path currently ends an input-required job with a generic start-a-new-request notice, and reactions to durable placeholders do not call A2A `CancelTask`. Exercise the persisted boundaries with the dedicated process-level SIGKILL fixture; its pass is hard-crash workflow evidence, not node-loss evidence or a production recovery-time objective.

## Consequences

1. A successful appservice acknowledgement no longer depends on an unrecorded in-memory delegation. A replacement can resume unstarted work, poll a known task, or replay a pending Matrix projection under a fenced lease.
1. A2A remains deliberately at-most-once at the unknown-ack boundary. Some work may have completed remotely while the room receives an ambiguity notice; operators must correlate the deterministic message ID with target-side evidence instead of asking the bridge to risk a duplicate side effect.
1. Postgres now holds content-bearing recovery state only for configured non-terminal capacity, plus content-free terminal refusal and completion evidence until cleanup. Transaction bytes, per-room/global active backlog, and active concurrency are bounded independently. Operators must still size the database for tombstone retention, alert on job age/state and `queue_full`, restrict backups/WAL, and understand that terminal live-row scrubbing does not retroactively erase backups.
1. Matrix event creation is idempotent per deterministic transaction ID. Media uploads themselves can repeat before the stable artifact event is sent, so the content repository can retain an unreferenced duplicate upload until its own cleanup.
1. D17 supersedes the process-local queue and event-marker completion portions of D3 and D4 while preserving D7's bounded `queue_full` contract in durable admission. The 32-per-room/256-global defaults now count non-terminal ledger jobs rather than process memory; a refusal is terminal evidence rather than an unrecorded drop. D5's `(room, agent)` context key and D9's `GetTask`/Matrix-edit protocol remain unchanged.
1. [ADR 0012](0012-bridge-decomposition-surface-budget.md)'s single-binary surface budget remains; this decision adds no broker or sibling service. Its process-local ordering premise is replaced by the ledger, while the single ready intake replica remains an availability boundary.
