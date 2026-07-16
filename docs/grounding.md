---
type: Specification
title: Sovereign Grounding Store
description: The composed CloudNativePG and pgvector schema, ACL metadata, and exact-ranking contract for grounded agents.
---

# Sovereign Grounding Store

Fgentic owns the `knowledge` database and ingestion contracts and composes storage from the existing shared CloudNativePG operand plus pgvector, layout-aware parsing from Docling, and embeddings from the model boundary behind agentgateway. This is not a bundled RAG platform: no second database operator, vector-database service, remote document API, retrieval API, connector catalog, or user interface is introduced. [Issue #332](https://github.com/fmind-ai/fgentic/issues/332) owns bounded batch ingestion and [issue #333](https://github.com/fmind-ai/fgentic/issues/333) owns permission-aware retrieval. [D18](design-decisions.md#d18--permission-aware-retrieval-binds-the-projected-identity-and-output-audience) and [ADR 0017](adr/0017-permission-aware-identity-binding.md) remain the identity and output-audience authority.

The store stays inside the existing PostgreSQL backup, failover, and connection-policy boundary established by [ADR 0007](adr/0007-shared-postgres-db-per-service.md). The pgvector-bearing PostgreSQL 17.10 operand supplies pgvector 0.8.5 while retaining the existing Barman backup tooling. Adding an empty database, roles, extension, schema, and indexes creates no new Pod, StatefulSet, steady connection, or steady-state memory reservation.

## Owned schema

`knowledge.chunks` is the stable storage boundary:

| Column      | Type           | Contract                                                                     |
| ----------- | -------------- | ---------------------------------------------------------------------------- |
| `chunk_id`  | `text`         | Trimmed non-empty stable primary key, at most 512 UTF-8 bytes.               |
| `content`   | `text`         | Non-whitespace source text, at most 65,536 UTF-8 bytes.                      |
| `embedding` | `vector(1024)` | Non-null, non-zero BGE-M3-profile embedding.                                 |
| `metadata`  | `jsonb`        | Strictly validated source, classification, and row ACL; text form ≤65,536 B. |

The dimension is a schema invariant matching the intended BGE-M3 embedding profile; it is not a per-cluster setting.

The write boundary validates all metadata before a row becomes queryable:

1. `source` is an object with required `id` and only the optional `title`, `locator`, `revision`, and `location` fields. `id` is a trimmed non-empty UTF-8 string of 1–512 bytes. Optional values, when present, are trimmed non-empty strings: `title` is at most 512 bytes, `locator` at most 2,048, `revision` at most 255, and `location` at most 512. `locator` identifies the source URI or path; `location` identifies the chunk-local page, section, or anchor. No other source key is admitted.
1. `classification` is exactly `public`, `approved_non_public`, `restricted`, `regulated`, `secret`, or `authentication`. Missing and unknown values fail. Classification is a ceiling, never an access grant.
1. `allowed_principals` is an array of at most 64 unique typed objects. A native identity is exactly `{"kind":"matrix","principal":"<full MXID>"}`. A bridged identity is exactly `{"kind":"bridged_matrix","network":"<DNS-1123 label>","principal":"<full MXID>"}`. Unknown keys, localparts, malformed MXIDs, a missing bridged network, and a network on a native Matrix identity fail.
1. `allowed_groups` is an array of at most 64 unique exact `partner/<policy-id>/<group-id>` strings whose two variable segments are DNS-1123 labels. Bare client IDs, local groups, prefixes, wildcards, and malformed segments fail.
1. At least one of `allowed_principals` or `allowed_groups` is non-empty. Public rows are not globally readable merely because their classification is `public`.

### Native Matrix audience

Every effective reader appears in the principal array. The secure predicate requires the complete projected array, so this example admits the two named readers together rather than either user independently standing in for the room audience.

```json
{
  "source": {
    "id": "handbook/access-control",
    "title": "Access control handbook",
    "locator": "https://docs.org-a.example/handbook/access-control",
    "revision": "sha256:7cdd7c88c1866a93",
    "location": "section-4.2"
  },
  "classification": "approved_non_public",
  "allowed_principals": [
    { "kind": "matrix", "principal": "@alice:org-a.example" },
    { "kind": "matrix", "principal": "@bob:org-b.example" }
  ],
  "allowed_groups": []
}
```

### Bridged Matrix principal

The bridge-owned network label is part of the identity. The same MXID under native `matrix` kind is a different principal and does not match.

```json
{
  "source": {
    "id": "support/incidents/2026-0715",
    "title": "Resolved support incident",
    "locator": "matrix:!support:org-a.example/$event",
    "revision": "event-$event",
    "location": "message-3"
  },
  "classification": "approved_non_public",
  "allowed_principals": [
    {
      "kind": "bridged_matrix",
      "network": "slack",
      "principal": "@slack_alice:org-a.example"
    }
  ],
  "allowed_groups": []
}
```

### Partner service group

Direct partner A2A has no trustworthy Matrix principal. It uses only groups projected from the exact operator-owned `(issuer, audience, azp)` policy.

```json
{
  "source": {
    "id": "partner/org-b/product-brief",
    "title": "Joint product brief",
    "locator": "https://partner.example/briefs/product",
    "revision": "v3",
    "location": "launch-window"
  },
  "classification": "public",
  "allowed_principals": [],
  "allowed_groups": ["partner/org-b-a2a/product"]
}
```

## In-cluster ingestion contract

`infra/knowledge/` is a structurally opt-in Flux layer. The root and shared DAG both point at `profiles/disabled`, which renders zero objects. Enable all three boundaries atomically by adding `../../infra/knowledge/cluster` to the environment Kustomization's `components` list:

```yaml
components:
  - ../base/provider-selection
  - ../../infra/knowledge/cluster
```

That Component selects `./infra/knowledge/profiles/enabled` and appends `components/knowledge-ingestion` to both the existing agentgateway and Postgres Flux Kustomizations. The agentgateway Component adds only the authenticated `:8082` listener; the Postgres Component owns the exact HBA row, DML-only `knowledge_ingestion` role, and immutable `knowledge-schema-v2` migration. Do not patch any of the three Flux objects independently.

Before adding the Component, the environment must have:

1. Generated and committed the optional scoped credentials with `FGENTIC_SECRET_SET=knowledge-ingestion mise exec -- scripts/gen-secrets.sh <server_name> <local|gcp>`.
1. Provisioned the operator-owned, read-only `knowledge-source-bundle` PVC in the restricted `knowledge` namespace. It contains `manifest.json` plus the sole source named by that manifest; unrelated filesystem entries are ignored and never snapshotted or exposed to Docling. [`source-bundle.example.yaml`](../infra/knowledge/source-bundle.example.yaml) shows a typed Matrix principal and [`source-bundle-partner.example.yaml`](../infra/knowledge/source-bundle-partner.example.yaml) shows the exact partner-group contract as a separate harmless public test fixture; checked-in ConfigMaps are never an operational path for non-public source bytes.
1. Reconciled the #340-owned `knowledge-embeddings.models.svc.cluster.local:8000` runtime and its complete proxy-to-model NetworkPolicy edge.
1. Verified the dedicated `agentgateway-proxy` embeddings listener and policy are accepted before unsuspending the CronJob.

The base CronJob is suspended. Enabling the layer does not parse a document or spend a model call by itself. A cluster overlay explicitly chooses the reviewed execution schedule and toggles `spec.suspend`; #335 owns future connector checkpoints and incremental triggering.

For teardown, first suspend the CronJob and wait until no ingestion Job is active. Remove the cluster Component and reconcile `knowledge-ingestion`, `agentgateway`, and `postgres`; those reconciliations prune the workload and dedicated listener, then remove the ingestion HBA row. Only after that fail-closed database edge is gone may the optional ciphertext and source bundle be removed.

### Source manifest

The manifest is strict UTF-8 JSON with duplicate-key rejection and this top-level shape:

```json
{
  "schema_version": 1,
  "corpus": "reference-docs",
  "sources": [
    {
      "path": "handbook.pdf",
      "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
      "source": {
        "id": "reference-docs/handbook",
        "title": "Reference handbook",
        "locator": "git:docs/handbook.pdf",
        "revision": "sha256:reviewed-source-revision"
      },
      "classification": "approved_non_public",
      "allowed_principals": [
        { "kind": "matrix", "principal": "@alice:org-a.example" }
      ],
      "allowed_groups": []
    }
  ]
}
```

Each ingestion run accepts exactly one source with one contained relative path, a required lowercase `sha256:<64 hex>` content digest, an ID namespaced below `corpus`, exact provenance, one known classification, normalized typed principals and/or exact partner groups, and at least one authorization operand. `source.revision` remains provenance and never substitutes for the verified byte digest. A second source is rejected; #335 owns sequencing independently checkpointed source bundles without widening one parser's read boundary. Unknown fields fail at every nesting level. The validator rejects duplicate keys, principals, and groups; absolute or escaping paths; unsupported formats; malformed/localpart Matrix IDs; kind/network mismatches; wildcard partner groups; non-canonical, duplicate, encrypted, symlink, or special office-archive entries; digest mismatches; and all configured byte, page, chunk, and total-count overflows.

The complete manifest and source validate before Docling is constructed. The snapshot phase reads the PVC through a read-only mount, selects only the declared source, verifies its digest before and after copying it into a private `emptyDir`, and hard-links those immutable bytes into one parser-only directory.

### Trust phases and data flow

One restricted Pod runs sequential phases with different mounted credentials:

```text
immutable source bundle
  -> trusted bounded snapshot
  -> one isolated Docling conversion + HierarchicalChunker
  -> trusted binder reconstructs IDs + exact ACL/provenance
  -> DML-only plan reuses unchanged vectors under one expiring writer lease
  -> authenticated model-local :8082 /tokenize preflight for every unique miss
  -> bounded :8082 /v1/embeddings batches
  -> one durable database checkpoint and filesystem ack per validated batch
  -> one transaction upserts the complete bound set and deletes stale rows
     only for sources successfully present in this manifest
```

The Docling init container receives the read-only runtime, one parser `subPath` containing exactly one normalized `document.<suffix>`, one raw-output `subPath`, and a separate 1 GiB parser-temp volume. It receives neither the manifest, source PVC, snapshot root, nor work-volume root. The read-only root filesystem and dropped capabilities leave no other writable handoff. This container boundary prevents the parser from reading a second source or writing outside its dedicated result and temp directories.

Docling output is never authoritative security state. It may emit only contiguous ordinal and contextualized text into one exact `chunks.jsonl`. Source identity is therefore absent from the parser payload: a fresh minimal-Python binder re-reads the snapshotted manifest, requires that exact result inventory, binds it to the sole manifest source, assigns locations, reconstructs the exact classification/ACL/provenance, and derives IDs. PostgreSQL receives only binder output. A partial Docling conversion, zero chunks, missing/extra result, ordinal gap, identifier collision, embedding failure, or final-set mismatch leaves the prior corpus unchanged.

The chunk ID is a SHA-256 domain hash over `fgentic.chunk.v1`, the fixed `bge-m3-1024-v1` embedding profile, source ID, normalized contextualized-content digest, and duplicate occurrence within that source. ACL, classification, title, locator, revision, and ordinal are deliberately excluded: changing only security/provenance metadata updates every affected row while reusing the unchanged vector. A content or embedding-profile change produces a new ID and embedding. The writer aborts if an existing ID has different content or a different source.

### Tokenization, embeddings, and network boundary

The embedding client accepts only `http://agentgateway-proxy.agentgateway-system.svc.cluster.local:8082/v1/embeddings`, model `BAAI/bge-m3`, bounded batches, and the ingestion-specific Bearer credential mounted only into the embed phase. Before any embedding call, it sends every uncached unique input to the same authenticated listener's exact `/tokenize` route. That raw route reaches the tokenizer owned by the model-local BGE-M3 runtime; it is not a second tokenizer implementation.

Each tokenize request carries exactly the fixed model, one prompt, enabled special tokens, and disabled token-string output. The client requires the strict vLLM response inventory, `max_model_len: 8192`, `count == len(tokens) <= 8192`, bounded non-negative int64 token IDs, and a null `token_strs`; one malformed or oversized input rejects the whole preflight before the first embedding. Cached vectors skip tokenization; identical duplicate misses share one preflight and one embedding input. The embeddings response must return the same model and complete unique indexes; every vector is exactly 1,024 finite IEEE float32 values and non-zero.

After each validated embeddings batch, the client atomically publishes one `checkpoint.ready` JSONL containing only the fixed profile, exact content, and embedding for each unique input. A native restartable PostgreSQL init-sidecar holds only the scoped database credential plus the runtime and dedicated checkpoint volume. It validates and commits that subset through `checkpoint.sql`, then atomically renames the same file to `checkpoint.acked`. The embed phase applies vectors only after validating that ack and deletes it before publishing the next batch. A sidecar restart safely replays the same ready file; a later Pod for the same source may reuse an unexpired durable exact-content checkpoint. Timeout, disappearance, conflicting ack, or database rejection leaves the final output absent.

The checkpoint cache is recovery state, not a second corpus. Each row binds the exact source ID, profile, content digest, content, vector, creation time, and expiry. It is readable only for the same source, becomes ineligible 24 hours after creation, and is deleted immediately when that source commits successfully. Planning and checkpointing prune expired rows only after holding the single-writer lease, retain the active input's exact receipts first, and deterministically cap the table at 1,024 rows. A separate cache-GC CronJob remains active even while the main ingestion CronJob is suspended; it runs hourly under the DML-only role and removes expired rows from the live cache/query surface through bounded `gc.sql` within the 24-hour retry window plus one collector interval. PostgreSQL dead tuples, WAL, and backups follow the existing shared-database vacuum and backup-retention boundary rather than this live-cache TTL. This preserves one complete 512-chunk retry plus one interleaved complete source while bounding live sensitive-content exposure and table growth.

This closes repeat work after the client has received and durably checkpointed a vector; it does not claim globally exactly-once model charging. If the model accepts a request but the Pod dies before receiving and checkpointing the response, a client-only protocol cannot distinguish that completion from a lost request and a later run may repeat it. Eliminating that ambiguity requires a server-side idempotency contract at the embeddings boundary.

Port `8082` is a separate Gateway listener with exact `/tokenize` and `/v1/embeddings` routes, strict API-key authentication, request buffering, and a per-proxy ceiling of 1,024 requests per hour: one tokenization plus one worst-case single-input embedding request for every possible chunk, with no extra burst. The ingestion Pod cannot reach the generic chat/A2A/MCP listener on `8080` or the model Service directly. Its default-deny policy permits only DNS, the `platform-pg` Pods on 5432, and agentgateway on 8082. The embed phase holds no PostgreSQL or provider/model credential; the checkpoint sidecar holds no gateway credential or work volume. NetworkPolicy is Pod-scoped, so Docling still shares those three allowed network destinations without their credentials; recursive-DNS exfiltration after a parser compromise remains a residual threat. True parser network isolation requires moving that phase into a separate Pod/Job, not adding another standard NetworkPolicy to this Pod. Installed-CNI evidence, not YAML inspection alone, remains required before an environment unsuspends the workload.

### Idempotency and re-ingestion

Planning atomically acquires the single `chunks-v1` lease before reading existing rows. A concurrent run fails closed; an interrupted run leaves a bounded 35-minute staging receipt that a later run can reclaim only after expiry, plus source-scoped embedding checkpoints for the 24-hour retry window and at most one hourly GC interval. The final transaction verifies that embedding changed only the vector field, upserts only rows whose content/vector/metadata differ, and removes obsolete chunk IDs only inside source IDs explicitly and successfully parsed in this run. Omitting a source from a later manifest does not delete it; connector tombstones and source removal belong to #335.

An exact rerun produces the same IDs and no embedding requests. ACL/classification/provenance-only changes retain IDs, replace metadata exactly, and leave no old authorized row. Changed content embeds only new IDs, upserts the complete successful source set, and removes that source's replaced IDs in the same transaction.

## Authorization before ranking

The future permission-aware retrieval service in issue #333 is the only chunk-row ACL enforcement point. It validates the D18 projection, binds values as query parameters, and calls one of two invoker-rights SQL functions as `knowledge_retrieval`. Both require a non-zero query vector, accept only `allowed_classifications = ARRAY['public']` or the ordered `ARRAY['approved_non_public', 'public']`, require `max_results` in 1–100, and return `chunk_id text`, `content text`, `metadata jsonb`, and `cosine_distance double precision`:

1. `knowledge.search_authorized_matrix(query_embedding vector(1024), allowed_classifications text[], audience jsonb, max_results integer DEFAULT 10)` requires the complete typed Matrix output audience.
1. `knowledge.search_authorized_groups(query_embedding vector(1024), allowed_classifications text[], groups text[], max_results integer DEFAULT 10)` requires exact partner groups from the operator-owned policy.

The Matrix function uses this physical query shape:

```sql
WITH authorized AS MATERIALIZED (
  SELECT chunk_id, content, metadata, embedding
  FROM knowledge.chunks
  WHERE metadata ->> 'classification' = ANY (allowed_classifications)
    AND metadata -> 'allowed_principals' @> audience
)
SELECT
  chunk_id,
  content,
  metadata,
  embedding <=> query_embedding AS cosine_distance
FROM authorized
ORDER BY cosine_distance, chunk_id
LIMIT max_results;
```

The partner-group function has a separate shape rather than a mixed identity branch:

```sql
WITH authorized AS MATERIALIZED (
  SELECT chunk_id, content, metadata, embedding
  FROM knowledge.chunks
  WHERE metadata ->> 'classification' = ANY (allowed_classifications)
    AND metadata -> 'allowed_groups' ?| groups
)
SELECT
  chunk_id,
  content,
  metadata,
  embedding <=> query_embedding AS cosine_distance
FROM authorized
ORDER BY cosine_distance, chunk_id
LIMIT max_results;
```

There is no missing-header, malformed-projection, empty-audience/group, or unscoped fallback. The SQL functions admit no rows for invalid operands and bound either audience to at most 16 entries; the future retrieval service returns no chunks when it cannot validate a projection before the call. Matrix uses containment of the complete effective-reader array; partner A2A uses intersection with its namespaced group array. The two identity kinds never share a function.

## Index and plan contract

The indexes have separate jobs:

| Index                       | Purpose                                                                        |
| --------------------------- | ------------------------------------------------------------------------------ |
| `chunks_classification_idx` | B-tree prefilter for the exact classification expression.                      |
| `chunks_principals_gin_idx` | GIN containment support for the typed `allowed_principals` array.              |
| `chunks_groups_gin_idx`     | GIN overlap support for exact partner-group strings.                           |
| `chunks_source_id_idx`      | Stable source-ID lookup for idempotent ingestion, replacement, and provenance. |
| `chunks_embedding_hnsw_idx` | Cosine HNSW evidence for a separate, non-authorizing vector-only query.        |

The secure plan must show the classification/ACL indexes filtering the materialized CTE and an exact sort over only that result. `chunks_embedding_hnsw_idx` is deliberately absent from that path. pgvector applies dynamic filters after an approximate HNSW candidate scan; using HNSW first could omit a nearer authorized row and cannot establish row authorization. Separate test evidence may prove HNSW eligibility for a vector-only query, but that query is never a retrieval authorization path.

## Roles, credentials, and namespace budget

The `knowledge` database has three non-superuser roles:

1. `knowledge_owner` owns only the schema and immutable migration objects. It is a trusted migration identity, not an application credential. Its role attributes and fail-closed HBA pairing prevent it from creating roles/databases, becoming a superuser, or acquiring authority outside `knowledge`.
1. `knowledge_ingestion` is introduced only by `components/knowledge-ingestion`. It receives public-schema usage for the pgvector type/functions, knowledge-schema usage, DML on `knowledge.chunks`, narrow staging/lease DML, and execution of only the metadata-validation functions required by constraints. It has no schema creation, migration-table, retrieval-function, role, database, or cross-database authority.
1. `knowledge_retrieval` receives only read/execute privileges inside the knowledge database. It neither owns nor mutates the schema and has no access to Synapse, MAS, bridge, kagent, Keycloak, or external-bridge databases. The functions are invoker-rights query primitives, not database row-level security: issue #333 must prove that the retrieval service exposes no unfiltered query path.

CloudNativePG admits only the exact TLS database/role pairs before its fail-closed HBA rules. The ingestion and retrieval credentials each have matching `postgres` and restricted `knowledge` namespace copies through separate coherent SOPS sets. The owner credential never leaves `postgres`. The ingestion gateway key is separate from all database and model/provider credentials.

The namespace uses the existing `small` ResourceQuota and container defaults. These are admission ceilings and default omitted requests/limits, not reservations. One run accepts one source of at most 16 MiB and at most 512 chunks. Raw and bound JSONL are each capped at 64 MiB. Vector-bearing plan/final JSONL use a separate derived ceiling that reserves 32 serialized bytes for every float32 value in all 512 chunks; the 320 MiB work volume covers only raw, pending, plan, and final artifacts plus headroom. Docling receives a separate 1 GiB parser-temp `emptyDir`, and the single-flight checkpoint volume is independently capped at 2 MiB. The versioned schema migration is a constrained one-shot Job; the database itself remains in the shared `postgres` namespace and creates no knowledge-specific stateful workload.

## Embedding upgrades and re-ingestion

Embedding vectors from different models, revisions, or dimensions are not comparable. Changing `bge-m3-1024-v1`, the fixed 1,024 dimensions, or chunk-normalization semantics requires a new versioned ID/profile contract, a versioned schema migration when storage changes, full corpus re-embedding, rebuild of every vector index, validation of ACL/source metadata under the target schema, and an explicit consumer cutover. Do not expose dimension or model as an environment override and do not mix old/new vector spaces in `knowledge.chunks`.

The migration must remain recoverable under the shared CNPG backup policy. Keep the old schema readable until the replacement corpus and exact/HNSW plan evidence pass, then cut over atomically and retire the obsolete data under an operator-approved retention window. A green manifest render proves neither corpus completeness nor semantic equivalence after re-embedding.
