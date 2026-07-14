---
type: Evidence
title: Bridge Performance Evidence
description: Dated, reproducible load-sanity evidence for the bridge; the regression reference for §12.5.
---

# Bridge Performance Evidence

This note records dated, reproducible evidence for the [bridge load-sanity requirement (§12.5)](bridge.md). It is a regression reference for the resource-bounded integration fixture, not a production capacity claim.

## Reference run — 2026-07-11

`mise run test:load` passed against the real Synapse → appservice → A2A → Matrix reply path in an isolated kind cluster. The fixture used an SDK-backed A2A stub and the bridge's Postgres-backed event deduplication.

| Configuration          | Reference value |
| ---------------------- | --------------- |
| Mentions               | 100             |
| Rooms                  | 10              |
| Mentions per room      | 10              |
| A2A stub delay         | 2,000 ms        |
| Global concurrency cap | 16              |
| Heap gate              | 67,108,864 B    |
| Transaction ACK gate   | 10,000 ms       |

| Observed result                           | Reference value |
| ----------------------------------------- | --------------- |
| Peak A2A concurrency                      | 10              |
| Peak bridge in-flight delegations         | 10              |
| Peak sampled queue depth                  | 79              |
| Baseline heap allocation                  | 1,627,528 B     |
| Peak heap allocation                      | 10,003,288 B    |
| Peak heap in-use                          | 13,885,440 B    |
| Peak heap system bytes                    | 19,365,888 B    |
| Final heap allocation                     | 7,650,984 B     |
| Deduplicated replay events                | 100             |
| Concurrent Matrix mention send phase      | 6,818 ms        |
| Replayed appservice transaction ACK       | 13 ms           |
| Scenario elapsed time                     | 39,219 ms       |
| Full build, cluster, and scenario harness | 172 s           |
| Per-room start and completion FIFO        | `true`          |
| Exactly one reply per mention             | `true`          |

The bridge reached ten concurrent delegations because only one job may run per room; that remains below the global cap of 16. Both gated peak heap measures stayed below 64 MiB, and the dispatcher drained its queue and in-flight work to zero. Replaying all 100 events produced 100 deduplication skips, no extra A2A executions, and no duplicate replies. The 13 ms transaction acknowledgement stayed well below the 10 s gate.

## Re-running the regression

With Docker available, run from the repository root:

```bash
mise run test:load
```

The task builds the local fixture image, creates the disposable `fgentic-bridge-load` kind cluster, runs the reference scenario, and deletes the cluster. It fails on a missing or duplicate reply, an extra A2A execution, per-room start or completion reordering, a transaction acknowledgement over 10 s, concurrency above the configured cap, a non-draining dispatcher, or peak heap allocation/in-use above 64 MiB.
