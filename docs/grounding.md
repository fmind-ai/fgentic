---
type: Specification
title: Sovereign Grounding Store
description: The composed CloudNativePG and pgvector schema, ACL metadata, and exact-ranking contract for grounded agents.
---

# Sovereign Grounding Store

Fgentic owns the `knowledge` database and ingestion contracts and composes storage from the existing shared CloudNativePG operand plus pgvector, layout-aware parsing from Docling, and embeddings from the model boundary behind agentgateway. This is not a bundled RAG platform: no second database operator, vector-database service, remote document API, retrieval API, connector marketplace, or user interface is introduced. [Issue #332](https://github.com/fmind-ai/fgentic/issues/332) owns bounded batch ingestion, [issue #335](https://github.com/fmind-ai/fgentic/issues/335) adds exactly one Git/Markdown reference connector, and [issue #333](https://github.com/fmind-ai/fgentic/issues/333) owns permission-aware retrieval. The reference connector is a bounded source adapter, not a generic plugin framework or catalog. [D18](design-decisions.md#d18--permission-aware-retrieval-binds-the-projected-identity-and-output-audience) and [ADR 0017](adr/0017-permission-aware-identity-binding.md) remain the identity and output-audience authority.

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

`infra/knowledge/` is a structurally opt-in Flux layer. The root and shared DAG both point at `profiles/disabled`, which renders zero objects. Enable acquisition, ingestion, the embedding route, and the database additions atomically by adding `../../infra/knowledge/cluster` to the environment Kustomization's `components` list:

```yaml
components:
  - ../base/provider-selection
  - ../../infra/knowledge/cluster
```

That Component selects `./infra/knowledge/profiles/enabled`, which composes the existing ingestion workload plus the sole Git/Markdown acquisition runtime, and appends `components/knowledge-ingestion` to both the existing agentgateway and Postgres Flux Kustomizations. The agentgateway Component adds only the authenticated `:8082` listener; the Postgres Component owns the exact ingestion and connector HBA rows, DML-only `knowledge_ingestion` role, connector-state-only `knowledge_connector` role, and immutable `knowledge-schema-v2` and `knowledge-schema-v3` migrations. Do not patch any of the three Flux objects independently.

Before adding the Component, the environment must have:

1. Generated and committed the optional scoped credentials with `FGENTIC_SECRET_SET=knowledge-ingestion mise exec -- scripts/gen-secrets.sh <server_name> <local|gcp>`. The set contains separate connector and ingestion database passwords plus the ingestion-only agentgateway key.
1. Provisioned the operator-owned `knowledge-source-bundle` PVC in the restricted `knowledge` namespace under the deployment's reviewed storage, capacity, encryption, backup, retention, and access policy. Only the acquisition Pod mounts it writable; ingestion mounts it read-only, and Flux never adopts or prunes it. An operator may still place the #332 manual `manifest.json` and its sole source at the claim root when no connector inventory exists. [`source-bundle.example.yaml`](../infra/knowledge/source-bundle.example.yaml) shows a typed Matrix principal and [`source-bundle-partner.example.yaml`](../infra/knowledge/source-bundle-partner.example.yaml) shows the exact partner-group contract as a separate harmless public test fixture; checked-in ConfigMaps are never an operational path for non-public source bytes.
1. Replaced the deliberate `192.0.2.1/32` `kubernetes_api_egress_cidr` placeholder with the cluster's current API endpoint `/32` and set `kubernetes_api_egress_port` to the endpoint port as observed by Pod egress after `kubernetes.default` Service DNAT, then proved that exact tuple with the installed CNI. The repo-owned k3d endpoint is the `k3d-fgentic-server-0` container address on port `6443`; the GKE public endpoint returned by the provider API uses port `443`. A cluster recreate or endpoint rotation requires refreshing this value before acquisition resumes.
1. Reconciled the #340-owned `knowledge-embeddings.models.svc.cluster.local:8000` runtime and its complete proxy-to-model NetworkPolicy edge.
1. Verified the dedicated `agentgateway-proxy` embeddings listener and policy are accepted before unsuspending the CronJob.

The Git/Markdown acquisition CronJob is enabled only with this opt-in profile and checks the already reconciled Flux artifact every five minutes. It has no database, model, provider, or agentgateway credential, so acquisition cannot spend a model call. The separate ingestion CronJob remains suspended until the #340 embedding runtime and network edge are reconciled and an operator explicitly toggles `spec.suspend`; its five-minute schedule is offset by two minutes and applies at most one source action per Job. A no-change run reaches no parser or model phase, while acquisition may safely retain new immutable snapshots during suspension.

For teardown, first suspend both CronJobs and wait until no acquisition or ingestion Job is active. Confirm that no desired connector action remains unapplied, and retain or export the PVC first when rollback evidence is required. Remove the cluster Component and reconcile `knowledge-ingestion`, `agentgateway`, and `postgres`; those reconciliations prune the acquisition and ingestion workloads and the dedicated listener, then remove both optional database HBA rows. The operator-owned PVC remains untouched and must be removed separately only after those fail-closed edges are gone.

### Git/Markdown connector interface

The [`SourceConnector` protocol](../infra/knowledge/connectors/git_markdown.py) is deliberately smaller than a general connector SDK:

1. expose one immutable snapshot cursor and enumerate the complete desired source set in stable order;
1. fetch one enumerated source with verified bytes, stable identity and locator, content revision, and its complete source-owned ACL;
1. report deletion candidates only for previously applied IDs inside that connector's exact `corpus/connector/` ownership prefix.

Exactly one implementation exists: `git-markdown`. It reads the cluster's existing `flux-system/flux-system` `GitRepository`; there is no registration API, dynamic loading, marketplace, per-repository credential, or arbitrary URL input. A current merge becomes eligible only after source-controller reports the current object generation as `Ready=True` and publishes one status artifact with bounded revision, digest, URL, and size. The acquisition Pod has a short-lived projected service-account token and RBAC for one exact `get`. Its default-deny policy permits only DNS, the exact source-controller Pods, and the one environment-substituted Kubernetes API endpoint `/32` and port observed after Service DNAT; it cannot reach PostgreSQL, agentgateway, a model, or a provider.

Any future source requires a reviewed typed implementation plus explicit GitOps composition, identity, credential, ACL, boundedness, NetworkPolicy, and lifecycle evidence. It cannot appear through runtime registration or a plugin marketplace.

Every selected repository must contain one strict repo-wide `.fgentic/knowledge-acl.json`. Its one classification and normalized principal/group arrays apply uniformly to every lowercase `docs/**/*.md` file; per-file overrides and implicit public defaults do not exist. A valid empty inventory is authoritative so deleting the final document can tombstone its stored chunks. The connector rejects a missing or malformed manifest, an empty ACL, and any boundedness, archive, UTF-8, digest, path, or Git LFS-pointer violation before publishing state.

Acquisition verifies the Flux artifact bytes against status, then publishes source bytes as content-addressed blobs and the complete canonical source inventory under its inventory digest. Existing objects must match byte-for-byte; immutable inventories are retained, and `current.json` is atomically replaced only after every blob and the new inventory are durable. The Flux artifact digest remains separate evidence from the canonical inventory digest.

A newer `Ready=True` Flux artifact that is downloaded and verified but violates the connector, archive, or ACL contract atomically replaces `current.json` with a bounded content-free `artifact-rejected` marker. On the next active ingestion schedule, the connector-only database function records that exact revision/digest, invalidates claims and applied source tuples, and moves every connector chunk to the non-retrievable `authentication` classification. It never falls back to the manual bundle. A later valid complete inventory clears the block but must reapply every source before retrieval resumes. API or download unavailability alone is not evidence of an ACL revocation and retains the last valid state; operators must keep ingestion scheduling active whenever previously ingested connector content is expected to honor new source policy.

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

Each ingestion run accepts exactly one source with one contained relative path, a required lowercase `sha256:<64 hex>` content digest, an ID namespaced below `corpus`, exact provenance, one known classification, normalized typed principals and/or exact partner groups, and at least one authorization operand. `source.revision` remains provenance and never substitutes for the verified byte digest. A second source is rejected; the connector materializes exactly one database-claimed `present` action as this #332-compatible bundle, so multi-source acquisition does not widen one parser's read boundary. Unknown fields fail at every nesting level. The validator rejects duplicate keys, principals, and groups; absolute or escaping paths; unsupported formats; malformed/localpart Matrix IDs; kind/network mismatches; wildcard partner groups; non-canonical, duplicate, encrypted, symlink, or special office-archive entries; digest mismatches; and all configured byte, page, chunk, and total-count overflows.

The complete manifest and source validate before Docling is constructed. The snapshot phase reads the PVC through a read-only mount, selects only the declared source, verifies its digest before and after copying it into a private `emptyDir`, and hard-links those immutable bytes into one parser-only directory.

### Trust phases and data flow

The connector-backed path adds acquisition and durable dispatch ahead of the unchanged one-source trust phases:

```text
Git merge
  -> Flux source-controller publishes a current Ready GitRepository artifact
  -> acquisition CronJob verifies and publishes immutable blobs + inventory
  -> ingestion CronJob publishes the complete desired snapshot to PostgreSQL
  -> one exact present or tombstone action is claimed for this ingestion run
  -> one immutable source bundle, or one exact source deletion
```

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

The client validates HTTP response framing before it decodes semantic JSON. A response may carry exactly one decimal `Content-Length`, exactly one `Transfer-Encoding: chunked`, or neither when the connection close delimits the body; declared length and transfer coding may never coexist. Optional whitespace around a declared framing value is limited to HTTP space and horizontal tab. Duplicate, malformed, unsupported, oversized, and incomplete declared framing fails closed. Tokenizer responses are capped at 256 KiB and embedding responses at 2 MiB whether the body is declared or streamed. The deterministic `test:knowledge-ingestion` suite proves these client-side transport cases offline; it does not replace live agentgateway reachability or NetworkPolicy acceptance.

Each tokenize request carries exactly the fixed model, one prompt, enabled special tokens, and disabled token-string output. The client requires the strict vLLM response inventory, `max_model_len: 8192`, `count == len(tokens) <= 8192`, bounded non-negative int64 token IDs, and a null `token_strs`; one malformed or oversized input rejects the whole preflight before the first embedding. Cached vectors skip tokenization; identical duplicate misses share one preflight and one embedding input. The embeddings response must return the same model and complete unique indexes; every vector is exactly 1,024 finite IEEE float32 values and non-zero.

After each validated embeddings batch, the client atomically publishes one `checkpoint.ready` JSONL containing only the fixed profile, exact content, and embedding for each unique input. A native restartable PostgreSQL init-sidecar holds only the scoped database credential plus the runtime and dedicated checkpoint volume. It validates and commits that subset through `checkpoint.sql`, then atomically renames the same file to `checkpoint.acked`. The embed phase applies vectors only after validating that ack and deletes it before publishing the next batch. A sidecar restart safely replays the same ready file; a later Pod for the same source may reuse an unexpired durable exact-content checkpoint. Timeout, disappearance, conflicting ack, or database rejection leaves the final output absent.

The checkpoint cache is recovery state, not a second corpus. Each row binds the exact source ID, profile, content digest, content, vector, creation time, and expiry. It is readable only for the same source, becomes ineligible 24 hours after creation, and is deleted immediately when that source commits successfully. Planning and checkpointing prune expired rows only after holding the single-writer lease, retain the active input's exact receipts first, and deterministically cap the table at 1,024 rows. A separate cache-GC CronJob remains active even while the main ingestion CronJob is suspended; it runs hourly under the DML-only role and removes expired rows from the live cache/query surface through bounded `gc.sql` within the 24-hour retry window plus one collector interval. PostgreSQL dead tuples, WAL, and backups follow the existing shared-database vacuum and backup-retention boundary rather than this live-cache TTL. This preserves one complete 512-chunk retry plus one interleaved complete source while bounding live sensitive-content exposure and table growth.

This closes repeat work after the client has received and durably checkpointed a vector; it does not claim globally exactly-once model charging. If the model accepts a request but the Pod dies before receiving and checkpointing the response, a client-only protocol cannot distinguish that completion from a lost request and a later run may repeat it. Eliminating that ambiguity requires a server-side idempotency contract at the embeddings boundary.

Port `8082` is a separate Gateway listener with exact `/tokenize` and `/v1/embeddings` routes, strict API-key authentication, request buffering, and a per-proxy ceiling of 1,024 requests per hour: one tokenization plus one worst-case single-input embedding request for every possible chunk, with no extra burst. The ingestion Pod cannot reach the generic chat/A2A/MCP listener on `8080` or the model Service directly. Its default-deny policy permits only DNS, the `platform-pg` Pods on 5432, and agentgateway on 8082. The embed phase holds no PostgreSQL or provider/model credential; the checkpoint sidecar holds no gateway credential or work volume. NetworkPolicy is Pod-scoped, so Docling still shares those three allowed network destinations without their credentials; recursive-DNS exfiltration after a parser compromise remains a residual threat. True parser network isolation requires moving that phase into a separate Pod/Job, not adding another standard NetworkPolicy to this Pod. Installed-CNI evidence, not YAML inspection alone, remains required before an environment unsuspends the workload.

### Idempotency and re-ingestion

Planning atomically acquires the single `chunks-v1` lease before reading existing rows. A concurrent run fails closed; an interrupted run leaves a bounded 35-minute staging receipt that a later run can reclaim only after expiry, plus source-scoped embedding checkpoints for the 24-hour retry window and at most one hourly GC interval. The final transaction verifies that embedding changed only the vector field, upserts only rows whose content/vector/metadata differ, and removes obsolete chunk IDs only inside source IDs explicitly and successfully parsed in this run. Omitting a source from a later manual #332 manifest still does not delete it.

For the connector path, the connector-only role stages the complete inventory before any action becomes eligible. PostgreSQL recomputes the canonical inventory digest over every exact source ID, path, source revision, content digest, ACL digest, and metadata object and verifies the declared source count. Only that complete enumeration may publish desired per-source state or turn a previously applied connector-owned source into a tombstone. Desired and applied source tuples remain separate, as do desired and applied snapshot cursors; a partial, corrupt, or stale enumeration cannot authorize deletion or advance a cursor. A newer complete inventory preempts older pending desired state, clears its claims, and immediately quarantines every now-stale ACL, content, or deletion delta. The last fully applied cursor remains audit evidence until all actions for the latest inventory converge.

The ingestion role reclaims an expired claim and selects at most one exact `present` or `tombstone` action for each Job. When a complete inventory changes a source's content, ACL, provenance, or presence, PostgreSQL immediately moves its old chunks to the valid but non-retrievable `authentication` classification; the two secure search functions accept only the explicit public classification arrays, so stale content or ACLs disappear before ranking while the sound embedding remains reusable. A present action materializes only the claimed content-addressed blob, verifies it against the claimed digest and immutable inventory, then atomically upserts its final chunks, records the applied source tuple, releases the claim, and attempts cursor advancement in the same transaction. A tombstone holds the same `chunks-v1` lease and atomically deletes all chunks and embedding-cache rows for that exact source ID, records the applied tombstone, releases the claim, and attempts the same advancement. The snapshot cursor advances only when every source in the complete inventory is applied exactly and no formerly applied present source remains outside it. A failed or expired action is retried without advancing state and stays unavailable through the authorized search boundary; publishing a newer inventory invalidates an older action, while replaying the same desired inventory is a no-op that preserves its active claim.

An exact rerun produces the same IDs and no embedding requests. For Git/Markdown, each source revision is its content digest and its locator remains the stable `GitRepository` identity plus path, so a repository artifact revision alone leaves unchanged documents untouched. ACL/classification-only changes retain IDs, replace metadata exactly, reuse unchanged vectors, and leave no old authorized row. Changed content embeds only new IDs, upserts the complete successful source set, and removes that source's replaced IDs in the same transaction. Tombstones make no embedding request.

### Evidence boundary

The supported #335 operational chain is:

1. merge reviewed `docs/**/*.md` content and its repo-wide ACL;
1. wait for Flux to report a current ready `GitRepository` artifact;
1. observe the acquisition schedule publish the matching immutable inventory and blobs;
1. with the shipped #340 embeddings profile (delivered via #540) enabled and its runtime Ready — source availability has landed, but live enablement and the end-to-end ingestion capture remain unproven — and ingestion deliberately unsuspended, let successive ingestion schedules apply one action each until the database cursor matches that inventory;
1. verify connector desired/applied state, resulting `knowledge.chunks`, and direct calls to the secure ACL-filtering SQL functions.

That is storage and direct secure-SQL evidence, not answer-level grounding evidence. The permission-aware retrieval boundary in #333 and the grounded-answer acceptance work in #336 must pass before this flow is described as an end-user answer path.

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

The `knowledge` database has four non-superuser roles:

1. `knowledge_owner` owns only the schema and immutable migration objects. It is a trusted migration identity, not an application credential. Its role attributes and fail-closed HBA pairing prevent it from creating roles/databases, becoming a superuser, or acquiring authority outside `knowledge`.
1. `knowledge_connector` is introduced only by `components/knowledge-ingestion`. It can publish complete connector snapshots and inspect only snapshot-level status through the versioned schema's narrow table and functions; it cannot select inventory rows or desired/applied source state. It has no privilege on chunks, embeddings, embedding checkpoints, retrieval functions, schema objects, or another database. Only the ingestion Pod's inventory-publication phase receives this credential; the acquisition Pod has no database credential.
1. `knowledge_ingestion` is introduced only by `components/knowledge-ingestion`. It receives public-schema usage for the pgvector type/functions, knowledge-schema usage, DML on `knowledge.chunks`, narrow staging/lease DML, and execution of only the metadata-validation functions required by constraints. It has no schema creation, migration-table, retrieval-function, role, database, or cross-database authority.
1. `knowledge_retrieval` receives only read/execute privileges inside the knowledge database. It neither owns nor mutates the schema and has no access to Synapse, MAS, bridge, kagent, Keycloak, or external-bridge databases. The functions are invoker-rights query primitives, not database row-level security: issue #333 must prove that the retrieval service exposes no unfiltered query path.

CloudNativePG admits only the exact TLS database/role pairs before its fail-closed HBA rules. The connector, ingestion, and retrieval credentials each have matching `postgres` and restricted `knowledge` namespace copies through coherent SOPS sets; connector and ingestion are separate passwords in the same explicitly selected optional set. The owner credential never leaves `postgres`. The ingestion gateway key is separate from all database and model/provider credentials. The acquisition Pod instead receives only a short-lived projected Kubernetes token for its exact `GitRepository` read; that token is neither a source credential nor stored in SOPS.

The namespace uses the existing `small` ResourceQuota and container defaults. These are admission ceilings and default omitted requests/limits, not reservations. One run accepts one source of at most 16 MiB and at most 512 chunks. Raw and bound JSONL are each capped at 64 MiB. Vector-bearing plan/final JSONL use a separate derived ceiling that reserves 32 serialized bytes for every float32 value in all 512 chunks; the 320 MiB work volume covers only raw, pending, plan, and final artifacts plus headroom. Docling receives a separate 1 GiB parser-temp `emptyDir`, and the single-flight checkpoint volume is independently capped at 2 MiB.

The connector reuses an operator-owned writable PVC whose capacity and access mode support the reviewed acquisition/ingestion scheduling policy. It stores content-addressed source blobs and immutable inventories. Each acquisition remains bounded to a 32 MiB artifact, 64 MiB expanded archive, 512 selected sources, and 64 MiB selected source bytes; retained snapshots are not silently garbage-collected, so capacity exhaustion fails acquisition closed until an operator applies a reviewed retention action. The reference schedule applies one source every five minutes: a 512-source initial inventory therefore has a minimum convergence time of 42 hours 40 minutes. This conservative throughput is a capacity limit, not a security delay—new complete inventories preempt stale desired state and quarantine changed or deleted content immediately. Increase it only with measured parser/model capacity and a separately reviewed concurrency contract. The acquisition Job is bounded and transient, and its active schedule performs no parsing or model call. The versioned schema migration is a constrained one-shot Job; the database itself remains in the shared `postgres` namespace and creates no knowledge-specific StatefulSet.

Safe retention is an explicit maintenance window, not background garbage collection. Suspend both CronJobs and wait for zero Jobs; require a complete, unblocked enumeration, no active claims or desired/applied source deltas, and a database applied cursor equal to the valid `current.json` revision and inventory digest. Retain or export that current envelope and its referenced blobs first. Only then may an operator prune non-current snapshot envelopes and blobs unreferenced by the current inventory under the deployment's audit policy, before resuming acquisition and then ingestion. If any predicate cannot be proved, leave the PVC untouched.

## Embedding upgrades and re-ingestion

Embedding vectors from different models, revisions, or dimensions are not comparable. Changing `bge-m3-1024-v1`, the fixed 1,024 dimensions, or chunk-normalization semantics requires a new versioned ID/profile contract, a versioned schema migration when storage changes, full corpus re-embedding, rebuild of every vector index, validation of ACL/source metadata under the target schema, and an explicit consumer cutover. Do not expose dimension or model as an environment override and do not mix old/new vector spaces in `knowledge.chunks`.

The migration must remain recoverable under the shared CNPG backup policy. Keep the old schema readable until the replacement corpus and exact/HNSW plan evidence pass, then cut over atomically and retire the obsolete data under an operator-approved retention window. A green manifest render proves neither corpus completeness nor semantic equivalence after re-embedding.
