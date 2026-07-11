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
