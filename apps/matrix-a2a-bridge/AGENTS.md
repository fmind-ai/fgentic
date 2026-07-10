# AGENTS.md — matrix-a2a-bridge

A Go **Matrix Application Service** that lets humans (and agents) `@mention` an AI agent in a Matrix room and delegates the task to that agent's **A2A** endpoint (`message/send`; `tasks/get` polling + `m.replace` edits for long tasks), posting the reply back as the agent's ghost user (`m.notice`). Built on `mautrix/go` (the Matrix side, MPL-2.0 — keep `NOTICE` current) + `a2a-go` v2 (the A2A side, pinned to kagent's version). One static binary, no CGO; state lives in Postgres via `DATABASE_URL` (mautrix StateStore + contexts + event dedup — in-memory fallback is dev-only). Behavior spec: repo `SPEC.md` §5–§6; design rationale: SPEC §4 (D3–D10).

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

- `cmd/bridge/main.go` — entry point: config load, slog, state layer (Postgres or memory), appservice + event-processor wiring, graceful shutdown. Also `-generate-registration`.
- `internal/config/` — typed, env-parsed config (`caarlos0/env`); `Config`, `Load` (fail-fast validation, incl. timeouts/rates).
- `internal/matrixapp/` — builds the mautrix `AppService` (`CreateFull`, optional SQL StateStore), loads/generates the registration (`rate_limited: false`).
- `internal/a2aclient/` — thin wrapper over the `a2a-go` SDK: resolve AgentCard, `SendMessage`/`GetTask`, map the `Task | Message` sum type to a `Result` (text, contextId, taskId, terminal), stamp the Matrix sender as `X-User-Id` via a context-aware RoundTripper.
- `internal/bridge/` — the orchestration: resolve `@mention` targets (typed `m.mentions` + body fallback; own-homeserver check + per-agent sender policy), dedup by event ID, enqueue on the per-room FIFO dispatcher (bounded concurrency), rate-limit, call A2A, poll long tasks, reply/edit as ghost. `agents.go` loads the routing/allowlist map (`allowedServers`/`allowedSenders` globs).
- `internal/state/` — the `Store` interface: Postgres (shared dbutil pool, `bridge_contexts` + `bridge_processed_events`) and in-memory (dev).
- `chart/` — Helm chart (Deployment, Service, ConfigMap for the agent map, NetworkPolicy, ServiceAccount; `database.secretName` feeds `DATABASE_URL`).
- `deploy/` — the Flux unit (Namespace + HelmRelease) reconciled by the `bridge` Kustomization; CD pins the image digest here.
- `registration.example.yaml` / `agents.example.yaml` — templates for the two config files.

## Conventions

- Server name `fgentic.fmind.ai`; bot `@a2a-bridge`; ghosts `@agent-<name>` (exclusive appservice namespace). Agent A2A path: `/api/a2a/<namespace>/<name>`.
- Rooms are **unencrypted** by design (the crypto package is not wired) — see repo ADR 0008.
- A2A is **non-streaming** (`message/send`); route through agentgateway by default (`A2A_BASE_URL`).
- Errors as values, wrapped with `%w`; never ignore an `err`. Context first for I/O, with a deadline on every A2A call. `log/slog` (JSON).
- Definition of done: `mise run format` clean, `mise run check` no findings, `mise run test` green, new behavior covered by a test. Conventional Commits; no attribution.
