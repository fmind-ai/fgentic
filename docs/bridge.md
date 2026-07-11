# Bridge Specification (formerly SPEC §5, §6, §12)

## 5. Bridge State Management (as implemented)

1. **Storage:** the `bridge` CNPG database; one shared `dbutil` pool (pgx driver) backs both the mautrix SQL StateStore and the bridge's own tables. Schema creation is idempotent DDL at startup — two tables don't justify a migration framework.
1. **Tables:** `mx_*` (mautrix StateStore, schema owned/upgraded by mautrix); `bridge_contexts(room_id, ghost, context_id, updated_at, PK(room_id, ghost))`; `bridge_processed_events(event_id PK, processed_at)` pruned opportunistically past 24h.
1. **Semantics:** context rows are best-effort (loss degrades to a fresh conversation, never an error). Event dedup is insert-if-absent **before** dispatch, so Synapse's at-least-once transaction delivery collapses to effectively-once agent invocation; on a store error the bridge proceeds (a rare duplicate beats a dropped delegation).
1. **HA note:** with Postgres-backed dedup and per-room ordering keyed in one process, `replicas: 1` is the design point (the appservice protocol is single-consumer); resilience is homeserver-side transaction retry + fast pod rescheduling, not replicas.

## 6. Async Delegation (as implemented)

1. `HandleMessage`: drop own bot/ghost senders and anything but `m.text` (D8) → resolve targets under D6's rules → dedup by event ID (D4) → enqueue `(room, ghost, prompt)` per target; return immediately.
1. Worker (per-room FIFO, global cap): ensure ghost registered/joined → rate check (D7, polite `m.notice` on rejection) → **typing indicator** while the agent works (cleared on exit) → `message/send` with the `(room, ghost)` contextId, the Matrix sender in `X-User-Id`, and a `REQUEST_TIMEOUT` (60s) transport deadline.
1. Terminal `Message`/`Task`: post the extracted text (artifacts → status message → last agent turn) as the ghost — an **`m.notice` reply** to the original event.
1. Non-terminal `Task`: post a "⏳ working on it…" placeholder → poll `tasks/get` (2s → 15s exponential backoff, 3-error budget, overall `TASK_TIMEOUT` 10m) → **edit** (`m.replace`) the placeholder into the final answer.
1. Failures post a short, generic ghost message (internal endpoints/errors never leak into rooms) and log the full error; empty replies get an explicit "(the agent returned no content)".

## 12. Testing & Validation Ladder

1. **Unit — done:** 28 tests, race-enabled, covering target resolution (federation spoofing, sender policy, dedup), dispatch ordering/concurrency/shutdown, rate-limit config, state semantics, and the `Task|Message` result mapping incl. non-terminal tasks. `mise run check` (lint, vuln, format, secrets, manifests via kubeconform + helm template, terraform validate, actionlint, trivy config) is clean.
1. **Contract — next:** an in-process A2A server fixture using `a2asrv` (the SDK's server half) exercising `Message` vs terminal-`Task` vs working-`Task` results, contextId echo, and kagent's wire-version header behavior — the permanent tripwire for D10.
1. **Integration (local k3d):** Skaffold profile standing up Synapse (lightweight config or ESS), registering the appservice, running the bridge with Postgres, and driving a scripted `@mention → reply` via the client API. Runs in CI on a `kind` runner.
1. **E2E demo script (Phase 5):** the runbook-driven "enterprise showcase" path: bootstrap → login → room → mention → reply → one Grafana trace covering the full hop. The demo _is_ the acceptance test.
1. **Load sanity:** N concurrent mentions across M rooms proving D3's queue design (no txn timeouts, bounded memory, per-room order held).
