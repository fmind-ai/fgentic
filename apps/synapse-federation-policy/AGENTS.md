# AGENTS.md — synapse-federation-policy

App-level layout and conventions for coding agents. Root platform context is in [.agents/AGENTS.md](../../.agents/AGENTS.md); the federation design is [docs/federation.md](../../docs/federation.md) and the human-facing contract is this app's [README.md](README.md).

## Commands

Run every task from this directory via `mise --cd apps/synapse-federation-policy run <task>` (a bare `mise run <task>` at the repo root cannot see this app's `mise.toml`):

- `mise --cd apps/synapse-federation-policy run install` — sync the locked uv environment (`uv sync`).
- `mise --cd apps/synapse-federation-policy run check` — format, lint (ruff), type-check (ty), and dependency-audit (pip-audit); it also asserts the `LICENSE` matches the repo root and the lock file is current.
- `mise --cd apps/synapse-federation-policy run test` — pytest with branch coverage, `--cov-fail-under=100` (the parser is the trust boundary, so coverage does not ratchet down).
- `mise --cd apps/synapse-federation-policy run format` — ruff + dprint.

## What this app is

A **self-contained**, standalone Python module (`fgentic_federation_policy`) that is Fgentic's **fail-closed policy border for events received over Matrix federation**. It registers Synapse's `federated_user_may_invite` and `should_drop_federated_event` spam-checker callbacks and evaluates only **content-free event metadata** against a git-declared JSON policy. It targets the platform-pinned Synapse 1.156.0 callback contract on Python 3.13, uses only the standard library plus the host's Synapse module API, and has no separately installed runtime dependency — the deployed artifact is one Python source file mounted into the unchanged Synapse container.

## Trust boundary (fail closed)

Federation input is **untrusted**. Any ambiguity is a deny-all state, never a silent allow: unknown keys, duplicate JSON keys or list entries, non-UTF-8 input, oversized files, invalid values, an unreadable replacement, or an unexpected `version` all activate deny-all. `allowed_servers`/`allowed_event_types` are exact, lowercase, unique, non-empty — **no globs** — and the local server must be listed. Never weaken a decision to `NOT_SPAM` to make a test pass, and never log event content, sender localparts, or policy values: violations emit only the stable `fgentic_federation_policy_violation` prefix plus content-free metadata (reason, sender server, event type, room/event IDs, policy digest, invite rule, allowlist counts).

## Layout

- `src/fgentic_federation_policy/__init__.py` — the module: `FederationPolicyModule` callbacks, the strict policy parser, atomic hot-reload (source-metadata change detection, exponential backoff capped at 30s, once-per-streak error logging), and the content-free violation record.
- `tests/` — `test_federation_policy.py` (behavioural/contract) + `test_policy_properties.py` (Hypothesis property tests over the untrusted-input parser).
- `policy/policy.json` — the canonical git-declared deployment policy (version `1`, `allowed_servers`, `allowed_event_types`, `invite_rule` ∈ {`allow_from_allowed_servers`, `deny_all`}). Deployment assets are intentionally **not** part of the standalone wheel.
- `kustomization.yaml` — a namespace-neutral Kustomize Component generating the **immutable versioned source** ConfigMap (`fgentic-synapse-federation-policy-v1`, holding the module) and the **stable mutable** `policy.json` ConfigMap (`fgentic-federation-policy`). The immutable/versioned split lets Flux reconcile a git policy change without repackaging the callback code, and Synapse swaps a valid update in atomically with no restart. Federation orgs A/B project this Component; C intentionally does not.
- `pyproject.toml` / `uv.lock` — pinned uv project (Python 3.13, ruff, ty, pytest, pip-audit).

## Conventions

1. Python, type-safe, small composable units; parse untrusted input into trusted values at the boundary; fail closed on any parse ambiguity.
1. Standard library only for the runtime module — do not add a runtime dependency; the deployed artifact stays one mountable source file.
1. Keep the parser and the `policy.json` schema in lock-step with the README table and the Kustomize Component; the versioned-source / stable-`policy.json` split is a deployment contract, not an implementation detail.
1. Validation gates: `check` + `test` warning-free, 100% branch coverage. The Synapse callback-contract version (1.156.0) is a pin — changing it is a deliberate, reviewed step.
