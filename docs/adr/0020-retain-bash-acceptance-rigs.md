---
type: Architecture Decision Record
title: Retain Bash Acceptance Rigs Until a Measured Pilot Trigger
description: Keep the current ShellCheck-gated rigs and require measured failure, churn, and timing evidence before one independently reversible Go pilot.
---

# 0020 — Retain Bash Acceptance Rigs Until a Measured Pilot Trigger

Status: Accepted

Decision issue: [#491](https://github.com/fmind-ai/fgentic/issues/491)

## Context

Fgentic's shell layer is executable product infrastructure, not incidental glue. It renders and inspects manifests, drives disposable acceptance environments, enforces ownership before teardown, and preserves crash-recovery receipts. Go could provide typed Kubernetes objects, table-driven subtests, coverage, structured failure names, and controlled parallelism. The built-in [`testing` subtest model](https://go.dev/blog/subtests) supports those benefits directly; a Kubernetes runtime port could use the typed [`controller-runtime` client](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client) instead of parsing `kubectl` output.

The acute static-analysis gap has changed since this question was filed. [#490](https://github.com/fmind-ai/fgentic/issues/490) made shfmt and ShellCheck warning-free gates for every owned script, and #550 owns the remaining explicit info/style allowlist. Rewriting 10,000 lines while that cheaper control is still settling would combine language migration, test-runner migration, and acceptance-boundary changes in one high-risk program.

This ADR therefore measures the current repository before deciding. The evidence snapshot is commit `b90c1bd` on 2026-07-18. Counts use physical lines, visible commits and `git log --numstat` churn, and shfmt's Bash AST; command sites count call expressions whose executable is exactly `kubectl`, `jq`, or `yq`, excluding dependency checks and comments.

## Evidence

The repository now has 81 shell files and 27,383 lines. The six investigated files contain 10,361 lines (37.8%):

| File                                             |      Lines | Commits touching | Added + deleted | `kubectl` / `jq` / `yq` sites | Owning hosted check task (one observed run) |
| ------------------------------------------------ | ---------: | ---------------: | --------------: | ----------------------------: | ------------------------------------------- |
| `scripts/lib/demo-cluster.sh`                    |      1,516 |                8 |           2,302 |                   30 / 74 / 1 | `check:demo`, indirect — 44.07s             |
| `scripts/test-trivy-operator.sh`                 |      1,279 |                4 |           1,471 |                  45 / 18 / 14 | `check:trivy-operator` — 5.56s              |
| `scripts/test-mcp-governance.sh`                 |      1,100 |                6 |           1,202 |                   2 / 37 / 60 | `check:mcp-governance` — 62.73s             |
| `scripts/lib/federation-contract-constrained.sh` |      1,149 |                4 |           1,511 |                    2 / 16 / 5 | `check:federation`, shared — 64.19s         |
| `scripts/test-demo.sh`                           |        999 |               13 |           1,409 |                    4 / 9 / 13 | `check:demo`, shared — 44.07s               |
| `scripts/test-knowledge-store.sh`                |      4,318 |                4 |           8,514 |                  40 / 72 / 31 | `check:knowledge-store` — 8.82s             |
| **Total**                                        | **10,361** |           **39** |      **16,409** |           **123 / 226 / 124** | —                                           |

The command-site count is real complexity—473 static subprocess call sites—but it is not 473 equivalent defects or 473 lines a Go client deletes. The six files also contain 688 here-document lines that define fixtures, schemas, and runtime configuration. Their 121 loop nodes include inventory and assertion iteration as well as polling. Only 211 lines (2.0%) belong to explicitly named wait, retry, readiness, or timeout functions; 23 `sleep` sites and 9 `kubectl wait` sites corroborate that bounded bucket. The rest requires semantic translation, not helper deletion.

The runner premise is also different from a serial Bash index. Root `check` has 44 direct dependencies, 32 backed by repository shell scripts. mise resolves those as a DAG and runs independent dependencies in parallel, up to its job limit; this is the documented behavior of [`depends`](https://mise.jdx.dev/tasks/task-configuration.html#depends). CI deliberately serializes the outer `format` → `check` → `test` phases, but the 44 checks fan out internally.

Three clean hosted PR runs on 2026-07-18 measured root `mise run check` at 168.11s, 171.29s, and 176.24s (median 171.29s): [run 29639676013](https://github.com/fmind-ai/fgentic/actions/runs/29639676013), [run 29639384944](https://github.com/fmind-ai/fgentic/actions/runs/29639384944), and [run 29639017331](https://github.com/fmind-ai/fgentic/actions/runs/29639017331). Their complete CI jobs, including install, format, check, and test, took 311–338 seconds. In the latest sample `check:shell` was the 150.69s critical-path task; moving one 1,100-line rig would remove about 4% of the lint input, while the Trivy and knowledge contract tasks themselves completed in under nine seconds. A Go port has no demonstrated wall-clock return yet.

Failure granularity remains a valid weakness. A shell task reports its mise name plus the script's message, not a `go test` subtest and coverage location. The six files nevertheless contain 352 explicit `fail` call sites plus contextual `echo`/`printf` diagnostics, and no issue identifies an escaped defect caused by an ambiguous message. Better structure is a benefit to prove in a pilot, not evidence for a broad rewrite.

## Options considered

### Port the rigs to Go now

The target would be a self-contained `test/acceptance/` Go module, one package per contract, using `testing.T` subtests and `gotestsum`. Static contracts would decode typed JSON/YAML once. Runtime contracts would use a typed Kubernetes client for API operations, explicit contexts for deadlines, and cleanup registered through `t.Cleanup`. Existing `check:*` and `test:*` mise names would remain thin `go test -run` wrappers.

This shape is coherent, but it adds another Go module and Kubernetes dependency boundary before a pilot has shown that typed access removes enough subprocess and fixture logic. It also cannot safely begin with lifecycle code: a port of receipt recovery or teardown ownership must prove interruption behavior, not merely happy-path equivalence.

### Keep Bash and adopt Bats

[Bats](https://bats-core.readthedocs.io/en/stable/) would add named, isolated cases and machine-readable TAP while preserving shell fluency. It is the smaller experiment if failure granularity becomes the dominant pain. It does not add types or meaningful branch coverage, however, and each case's isolated process makes shared expensive cluster setup an explicit design problem. Refactoring the current state machines into Bats would still be migration work, while shfmt and ShellCheck already cover the immediate static risk.

### Keep the current rigs with measured revisit triggers

This preserves known behavior, task names, operator `bash -x` debugging, and the single shell helper dialect while #550 burns down the remaining lint debt. It accepts weak subtest and coverage signals until repository evidence justifies one bounded experiment.

## Decision

Do **not** port a rig to Go or Bats now. Retain Bash, the existing mise task names, shfmt, and ShellCheck. Do not file migration children from this ADR.

Revisit the decision only when one named rig satisfies at least two of these triggers over a rolling 90-day window:

1. Two behavior defects escape its current deterministic checks or two incidents require manual reconstruction that types or subtests would have prevented.
1. Ten clean hosted runs show the rig's owning task contributes a median of at least 60 seconds to the `check` critical path, not merely to parallel work hidden below it.
1. The rig receives at least ten behavior-changing commits and review repeatedly cannot isolate which contract changed.
1. An adopter, audit, or CI consumer requires machine-readable per-assertion results or coverage that the current task cannot supply.

When triggered, use this ranked pilot order:

1. **`test-mcp-governance.sh`.** It has 97 direct `jq`/`yq` sites, the longest observed candidate check task, and a disposable Docker runtime rather than cluster ownership. It offers the clearest typed-data and subtest experiment.
1. **`test-trivy-operator.sh`.** Its static assertions are bounded, but its second half owns a disposable k3d lifecycle and cleanup. The 5.56s check time makes performance a weak motive; port only for demonstrated correctness or reporting value.
1. **`test-knowledge-store.sh`.** It is now the largest file, but combines static YAML/SQL/ordering assertions with a 3,000-line kind/CNPG runtime contract. Split its contract conceptually before considering a language change; size alone is not a safe pilot criterion.
1. **`test-demo.sh`.** High change count makes it interesting, but it sources and probes `demo-cluster.sh`; a standalone port risks duplicating lifecycle semantics.
1. **`federation-contract-constrained.sh`.** It is a definition-only library with 89 functions sourced into the broader federation contract. It cannot be removed independently without redesigning that suite's composition.
1. **`demo-cluster.sh`.** Move last, if ever. It is shared by demo, dev, federation, and teardown tests and owns destructive artifact validation, atomic receipts, recovery, capacity modes, and operator diagnostics.

The first pilot must be one independently revertable PR and keep its public mise task name. During the pilot, old and new implementations run against the same positive and negative fixtures. The replacement is accepted only when it:

1. proves both static and runtime behavior under the designated runtime owner;
1. reduces implementation lines by at least 30% without a second one-off helper framework;
1. does not regress median wall-clock by more than 10%;
1. demonstrates a materially better injected-failure message naming the exact contract and expected/actual value; and
1. can be reverted without changing another acceptance rig or shared lifecycle helper.

Stop and revert the pilot if one quarter of the source is translated without a credible 30% reduction, if runtime parity requires a simultaneous shared-lifecycle rewrite, or if a new Kubernetes client dependency remains while equivalent `kubectl`/JSON parsing still dominates.

## Shared-helper boundary

`scripts/lib/` remains the only helper dialect for unported shell paths. A pilot may call an existing stable CLI boundary, but must not reimplement `demo-cluster.sh` ownership, receipt, wait, or teardown semantics in Go. Remove a self-contained old rig atomically only after parity; do not leave Bash and Go implementations selectable indefinitely.

Do not create a general Go acceptance-helper package for the first pilot. If two accepted ports reveal the same typed fixture or Kubernetes operation, extract the common package in the second PR with both consumers present. This prices the half-migrated world explicitly and avoids speculative dual helpers.

## Cost and consequences

These are planning ranges, not delivery estimates: a Bats granularity pilot is 16–32 agent-hours; the MCP Go pilot is 40–80 agent-hours including Docker parity and review; all six rigs are at least 240–480 agent-hours before runtime-owner scheduling. `demo-cluster.sh` alone plausibly consumes 80–160 hours because interruption and destructive cleanup states must be exercised. Every port temporarily doubles a gate that runs on all PRs and creates regression risk at exactly the boundary used to detect regressions.

The accepted cost is continued shell maintenance and weak coverage accounting. The benefit is no broad rewrite, stable operator diagnostics, preserved `bash -x` incident fluency, and a measurable threshold before adding a new module and helper system. #550 remains the immediate improvement path; a future pilot supplies before/after LOC, wall-clock, and failure-quality evidence rather than assuming them.
