---
name: bridge-dev
description: Develop the matrix-a2a-bridge Go appservice — setup, inner loop (skaffold or side-load into k3d), validation gates, and the licensing constraints. Use when changing code under apps/matrix-a2a-bridge/.
metadata:
  author: Médéric Hurier (Fmind)
  created: 2026-07-11
---

# Bridge Development Loop

The bridge is self-contained under `apps/matrix-a2a-bridge/` (own Go module, mise.toml, Dockerfile, Helm chart, Flux deploy unit). Architecture, package layout, and code conventions live in [its AGENTS.md](../../../apps/matrix-a2a-bridge/AGENTS.md) — read that first; this skill covers the loop around it.

## Setup & gates

1. Fresh checkout: `mise --cd apps/matrix-a2a-bridge install` (go mod tidy; the app pins its own Go toolchain, separate from the root's).
1. Root `mise run format` / `check` / `test` / `build` delegate into the app — running them at the repo root covers the whole monorepo, and it's exactly what hooks + CI run.
1. Definition of done: `format` clean, `check` no findings (golangci-lint, govulncheck, dprint, gitleaks), `test` green (gotestsum, `-race` + coverage), new behavior covered by a test.

## Developing a feature (fast inner loop)

1. **Iterate on the two hot packages, gate on the full suite.** `go test ./internal/a2aclient/ ./internal/bridge/` runs in seconds; the full `mise run test` adds the slow `matrixapp` + `-race` run. Run the full gate only before committing. (The first `go test` after adding a file spends time compiling — not a hang.)
1. **Recipe — add a per-agent policy/admission knob** (the shape of `extensions` #114, `maxCost` #142, `stage` #128):
   - `internal/bridge/agents.go` — add the field to `agentConfig` (on-disk YAML) **and** `AgentRef`; parse it in `compileAgent` (reject where it doesn't apply, e.g. remote-only fields on a local target); fold operational config into `mappingID(...)` so a change re-keys queued jobs and forces re-validation via `SameTarget`.
   - `internal/config/config.go` — only if the feature needs a global env knob (e.g. `STAGING_ROOMS`); add a `validate()` clause and wire it into the chart `deployment.yaml`/`values.yaml`.
   - `internal/bridge/handler.go` — enforce in `dispatchResolvedTarget`, respecting admission order: mapping/`SameTarget` → sender policy → **stage** → remote readiness → **cost** → limiter → A2A. A fail-closed refusal is a small `refuseXxx` helper emitting `outcome=denied, terminal_stage=admission, terminal_reason=<distinct>, a2a_attempted=false` and, when a bounded room notice is wanted, posting exactly once behind `allowNotice`. Add the new `terminal_reason` to `docs/audit.md`.
   - `agents.schema.json` (+ the local/remote `oneOf` `not` branches when the field is target-scoped) and `agents.example.yaml`; document the behavior in `docs/bridge.md` §6.
1. **Adding a method to the `a2aClient` interface** (handler.go) breaks compilation until **four** test fakes implement it: `scriptedA2AClient`, `deadlineA2AClient` (handler_test.go), `cardSequenceClient` (profiles_test.go), `tracingA2AClient` (telemetry_test.go).

## Test patterns to reuse (deterministic, no cluster)

1. **Remote A2A contract** — `newRemoteContractFixture` (a2aclient/remote_contract_test.go) runs an in-process `a2asrv` server and signs a card with a test P-256 key; mutate via `fixture.setCard`, resolve, then assert on `Result`, request headers, or `fixture.messages`. The deterministic substitute for a live remote.
1. **Delegation audit** — `setBridgeLogOutput(b, &out)` + `auditRecords(t, out.String())` capture the content-free `fgentic.delegation.v1` records; assert the exact `outcome`/`terminal_stage`/`terminal_reason`/`a2a_attempted` tuple. `TestDelegationAuditRecordIsStableAndContentFree` is the schema anchor — extend its `want` map when you add an audit field.
1. **Dispatch happy/deny path** — `pollingHarness(t, client)` wires a bridge to a mock Matrix server (`matrixRecorder`) + a `scriptedA2AClient`; call `dispatchResolvedTarget` directly and assert `client.callCount`, the audit, and posted notices. Pre-set `intent.Registered = true` + `StateStore.SetMembership(...MembershipJoin)` to skip network registration.
1. **Config / schema** — `agents_test.go` loads inline YAML via `LoadAgents`; `agents_schema_test.go` validates the example/chart/integration fixtures against `agents.schema.json`.

## Integration validation (when a change has a runtime surface)

1. `mise run test:integration` (root) — kind + real-Synapse driver (`test/integration/cmd/driver`) proving `@mention → A2A → reply`, dedup, rate limit, tampered-card fail-close. Needs Docker; ~4 min; slow/flaky under host contention, so CI's clean runner is authoritative. Extend `runBasic` to prove new end-to-end behavior. A `messageContent` with a nil `Mentions.UserIDs` serializes `"user_ids": null` → Synapse `M_BAD_JSON`; send `[]string{}` for a mention-less message.
1. `mise run check:federation` — **offline** federation contracts (signing, whitelist/policy/ACL, denied control); the deterministic proof for AgentCard-signing and revocation invariants without spinning up `fed:up`.

## Testing a change on the live local cluster (two paths)

1. **Inner loop — `mise run watch`** (skaffold dev, from the repo root): rebuilds the image on change and deploys the chart into ns `bridge` with the fresh digest. The rest of the platform (ESS, agentgateway, kagent, Postgres) must already be up via Flux (matrix-agents bootstrap runbook). Note skaffold temporarily takes over the Helm release from Flux — Flux reconverges to the git state afterwards.
1. **One-shot — `mise run bridge:load`**: builds `matrix-a2a-bridge:local` and imports it into k3d, then `kubectl -n bridge rollout restart deploy/matrix-a2a-bridge`. This works because the local overlay (`clusters/local/kustomization.yaml`) pins the bridge HelmRelease to `matrix-a2a-bridge:local` with `pullPolicy: Never` — local clusters never pull from GHCR.
1. Verify end-to-end with the matrix-agents "verify the flow" runbook (`@mention → A2A → reply`); logs: `kubectl -n bridge logs deploy/matrix-a2a-bridge`.

## Local run without a cluster

1. `mise --cd apps/matrix-a2a-bridge run watch` (air live-reload) with env config (`internal/config`); state falls back to in-memory when `DATABASE_URL` is unset — dev-only. `-generate-registration` produces a registration file; `registration.example.yaml` / `agents.example.yaml` are the templates.

## Shipping

1. Merge to main → `cd.yml` builds/scans/signs the multi-arch image and commits the immutable digest into `deploy/helmrelease.yaml` (`[skip ci]`) — never hand-edit that line, and `git pull --rebase` after a bridge merge (see the github-flow skill).
1. Chart changes ship through the same Flux `bridge` Kustomization; agent-map changes (ghost allowlist) go in chart values — see the matrix-agents "add an agent" runbook.

## Hard constraints

1. **Never add an AGPL dependency.** mautrix/go is MPL-2.0 — keep `NOTICE` current when touching deps ([docs/licensing.md](../../../docs/licensing.md)).
1. Only stable-spec appservice endpoints (homeserver portability); no crypto package (rooms unencrypted by policy, ADR 0008); A2A is non-streaming via the official `a2a-go` SDK — never hand-roll JSON-RPC.
1. Never match users by localpart without checking the homeserver (federation rule); keep per-sender/per-room rate limits intact (cost is a failure mode).
