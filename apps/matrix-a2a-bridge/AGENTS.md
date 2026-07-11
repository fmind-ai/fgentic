# AGENTS.md — matrix-a2a-bridge

A Go **Matrix Application Service** that lets humans (and agents) `@mention` an AI agent in a Matrix room and delegates the task to that agent's **A2A** endpoint (`message/send`; `tasks/get` polling + `m.replace` edits for long tasks), posting the reply back as the agent's ghost user (`m.notice`). Built on `mautrix/go` (the Matrix side, MPL-2.0 — keep `NOTICE` current) + `a2a-go` v2 (the A2A side, pinned to kagent's version). One static binary, no CGO; state lives in Postgres via `DATABASE_URL` (mautrix StateStore + contexts + event dedup — in-memory fallback is dev-only). Behavior spec: repo `docs/bridge.md` §5–§6; design rationale: `docs/design-decisions.md` (D3–D10).

## Commands (mise)

Run from this directory. `mise` is the single source of truth; lefthook + CI reuse it.

- `mise install` — `go mod tidy` (resolves go.sum; run this FIRST in a fresh checkout).
- `mise run watch` — live-reload dev server (air).
- `mise run format` — goimports, gofumpt, dprint.
- `mise run check` — golangci-lint, govulncheck, dprint check, gitleaks.
- `mise run test` — gotestsum with race + coverage.
- `mise run build` — compile `bin/matrix-a2a-bridge`; `mise run build:image` builds the distroless image.

Heavy CLIs (golangci-lint, gotestsum, gitleaks, dprint) are mise-managed; `goimports`/`gofumpt`/`govulncheck`/`air` are `go tool` via the `go.mod` tool directive.

## Layout

- `cmd/bridge/main.go` — entry point: config load, slog, state layer (Postgres or memory), appservice + event-processor wiring, intake-first bounded shutdown. Also `-generate-registration`.
- `internal/config/` — typed, env-parsed config (`caarlos0/env`); `Config`, `Load` (fail-fast validation, incl. timeouts/rates).
- `internal/matrixapp/` — builds the mautrix `AppService` (`CreateFull`, optional SQL StateStore), loads/generates the registration (`rate_limited: false`).
- `internal/a2aclient/` — thin wrapper over the `a2a-go` SDK: resolve uncached AgentCards for profile refresh, cache delegation clients, `SendMessage`/`GetTask`, map the `Task | Message` sum type to a `Result` (text, contextId, taskId, terminal), authenticate as the bridge workload, and stamp the Matrix sender plus W3C `traceparent` via a context-aware RoundTripper.
- `internal/evaluation/` + `cmd/eval/` — fixed, typed A2A quality suite for the three sample agents: deterministic rubrics, direct agentgateway Prometheus deltas, optional operator pricing, and mergeable JSON/Markdown reports under `.agents/tmp/`.
- `internal/bridge/` — the orchestration: resolve `@mention` targets (typed `m.mentions` + body fallback; own-homeserver check + per-agent sender policy), dedup by event ID, enqueue on the per-room FIFO dispatcher (bounded running-plus-queued capacity and concurrency), rate-limit, call A2A, poll long tasks, reply/edit as ghost, and emit content-free terminal records through the `log_stream=audit` logger. Queue overflow fails closed before admission with no reply or A2A. Duplicate deliveries produce audit evidence without another dispatch. `agents.go` loads and atomically replaces the routing/allowlist map (`bridgedOrigins`, `allowedServers`, and `allowedSenders` globs); bridge origins are anchored full-MXID namespaces and always require an explicit per-agent sender match. `profiles.go` derives Matrix ghost profiles from AgentCards while retaining the last-known card on refresh failures; `directory.go` serves the local, policy-filtered `!agents` command without A2A or LLM work.
- `internal/state/` — the `Store` interface: Postgres (shared dbutil pool, `bridge_contexts` + `bridge_processed_events`) and in-memory (dev).
- `internal/telemetry/` — env-gated OTLP/HTTP exporter setup; unset `OTEL_EXPORTER_OTLP_ENDPOINT` keeps standalone development tracing-free.
- `chart/` — Helm chart (Deployment, Service, ConfigMap for the agent map, NetworkPolicy, ServiceAccount; `database.secretName` feeds `DATABASE_URL`).
- `deploy/` — the Flux unit (Namespace + HelmRelease) reconciled by the `bridge` Kustomization; CD pins the image digest here.
- `registration.example.yaml` / `agents.example.yaml` — templates for the two config files.

## Conventions

- Server name `fgentic.fmind.ai`; bot `@a2a-bridge`; ghosts `@agent-<name>` (exclusive appservice namespace). Agent A2A path: `/api/a2a/<namespace>/<name>`.
- The appservice is a strict single consumer: Helm rejects `replicaCount != 1`, and Deployment updates use `Recreate` so two independent per-room dispatchers never overlap.
- Rooms are **unencrypted** by design (the crypto package is not wired) — see repo ADR 0008.
- A2A is **non-streaming** (`message/send`); route through agentgateway by default (`A2A_BASE_URL`).
- Dispatcher capacities count accepted running and queued work (defaults: 32 per room, 256 globally). Preserve typed, silent pre-admission overflow and its `queue_full` audit; do not turn overload into Matrix response amplification.
- Each of the four invocation/notice sender/room limiter maps is independently capped at 4096 buckets. Unknown keys fail closed at capacity; never evict an active bucket to admit churn because that resets its burst budget. Idle cleanup scans at most once per minute and only on a new key.
- Shutdown must stop bridge-owned HTTP intake, force-close timed-out transaction connections before the synchronous processor barrier, and then drain delegations for the configured 25-second grace under the chart's 45-second pod grace. Every accepted target gets a terminal audit, including queued drops; do not cancel the runtime context before acknowledged intake is ordered behind the barrier.
- The full projected agent ConfigMap directory must be mounted, never `agents.yaml` via `subPath`: the latter cannot receive Kubernetes atomic updates. Invalid reloads keep the last-known routing policy.
- Matrix display names and configured `mxc://` avatars are portable. Arbitrary description fields require Matrix v1.16 and are not consistently rendered by Element, so keep `!agents` as the user-facing metadata/status surface.
- Errors as values, wrapped with `%w`; never ignore an `err`. Context first for I/O, with a deadline on every A2A call. `log/slog` (JSON); stable audit records use the dedicated `log_stream=audit` child logger and must never include Matrix/A2A content bodies.
- Definition of done: `mise run format` clean, `mise run check` no findings, `mise run test` green, new behavior covered by a test. Conventional Commits; no attribution.
