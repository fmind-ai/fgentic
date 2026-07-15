---
type: Specification
title: Sovereign Grounding Store
description: The composed CloudNativePG and pgvector schema, ACL metadata, and exact-ranking contract for grounded agents.
---

# Sovereign Grounding Store

Fgentic owns the `knowledge` database contract and composes its storage from the existing shared CloudNativePG operand plus pgvector. This is not a bundled RAG platform: no second database operator, vector-database service, ingestion framework, embedding server, retrieval API, connector catalog, or user interface is introduced. [Issue #332](https://github.com/fmind-ai/fgentic/issues/332) owns ingestion and [issue #333](https://github.com/fmind-ai/fgentic/issues/333) owns permission-aware retrieval. [D18](design-decisions.md#d18--permission-aware-retrieval-binds-the-projected-identity-and-output-audience) and [ADR 0017](adr/0017-permission-aware-identity-binding.md) remain the identity and output-audience authority.

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

The `knowledge` database has two non-superuser roles:

1. `knowledge_owner` owns the schema and migration/ingestion objects, so it is a trusted write-boundary identity. Its role attributes and fail-closed HBA pairing prevent it from creating roles/databases, becoming a superuser, or acquiring authority outside `knowledge`.
1. `knowledge_retrieval` receives only read/execute privileges inside the knowledge database. It neither owns nor mutates the schema and has no access to Synapse, MAS, bridge, kagent, Keycloak, or external-bridge databases. The functions are invoker-rights query primitives, not database row-level security: issue #333 must prove that the retrieval service exposes no unfiltered query path.

CloudNativePG admits only the exact TLS database/role pairs before its fail-closed HBA rules. The retrieval credential is copied into the restricted `knowledge` namespace through the per-cluster SOPS flow; it is not shared with kagent, the bridge, or an ingestion workload.

The namespace uses the existing `small` ResourceQuota and container defaults. These are admission ceilings and default omitted requests/limits, not reservations. The versioned schema migration is a constrained one-shot Job; the database itself remains in the shared `postgres` namespace and creates no knowledge-specific stateful workload.

## Embedding upgrades and re-ingestion

Embedding vectors from different models or dimensions are not comparable. Changing the embedding model or the fixed 1,024 dimensions requires a versioned schema migration, full corpus re-embedding, rebuild of every vector index, validation of ACL/source metadata under the target schema, and an explicit consumer cutover. Do not expose dimension or model as an environment override and do not mix old/new vector spaces in `knowledge.chunks`.

The migration must remain recoverable under the shared CNPG backup policy. Keep the old schema readable until the replacement corpus and exact/HNSW plan evidence pass, then cut over atomically and retire the obsolete data under an operator-approved retention window. A green manifest render proves neither corpus completeness nor semantic equivalence after re-embedding.
