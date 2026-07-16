\set ON_ERROR_STOP on
\set VERBOSITY terse

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';
SET LOCAL idle_in_transaction_session_timeout = '60s';

SELECT set_config('fgentic.ingestion_run_id', :'run_id', false);

-- Lock/reclaim the lease before touching staging rows, matching checkpoint/write lock order. One
-- transaction-stable cutoff prevents a lease from changing state between cleanup decisions.
DELETE FROM knowledge.ingestion_leases
WHERE expires_at <= transaction_timestamp();

INSERT INTO knowledge.ingestion_leases (name, holder, expires_at)
VALUES (
  'chunks-v1',
  :'run_id'::uuid,
  transaction_timestamp() + interval '35 minutes'
);

-- A crashed run leaves bounded staging receipts. Once this transaction owns the single-writer
-- lease, it can remove only receipts without a lease that was live at the shared cutoff.
DELETE FROM knowledge.ingestion_pending AS pending
WHERE NOT EXISTS (
  SELECT 1
  FROM knowledge.ingestion_leases AS leases
  WHERE leases.holder = pending.run_id
    AND leases.expires_at > transaction_timestamp()
);
DELETE FROM knowledge.ingestion_final AS final
WHERE NOT EXISTS (
  SELECT 1
  FROM knowledge.ingestion_leases AS leases
  WHERE leases.holder = final.run_id
    AND leases.expires_at > transaction_timestamp()
);

-- Cache cleanup runs only after this transaction owns the single-writer lease.
DELETE FROM knowledge.ingestion_embedding_cache
WHERE expires_at <= clock_timestamp();

DELETE FROM knowledge.ingestion_pending
WHERE run_id = :'run_id'::uuid;
DELETE FROM knowledge.ingestion_final
WHERE run_id = :'run_id'::uuid;

\copy knowledge.ingestion_pending (payload) FROM '/work/pending.jsonl' WITH (FORMAT csv, DELIMITER E'\x1f', QUOTE E'\x1e', ESCAPE E'\x1e')

DO $validation$
DECLARE
  staged_count integer;
BEGIN
  SELECT count(*) INTO staged_count
  FROM knowledge.ingestion_pending
  WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid;

  IF staged_count NOT BETWEEN 1 AND 512 THEN
    RAISE EXCEPTION 'pending chunk set must contain between 1 and 512 rows';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_pending
    WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND (
        jsonb_typeof(payload) IS DISTINCT FROM 'object'
        OR NOT payload ?& ARRAY['chunk_id', 'content', 'metadata']
        OR payload - ARRAY['chunk_id', 'content', 'metadata'] <> '{}'::jsonb
        OR jsonb_typeof(payload->'chunk_id') IS DISTINCT FROM 'string'
        OR (payload->>'chunk_id') !~ '^sha256:[0-9a-f]{64}$'
        OR jsonb_typeof(payload->'content') IS DISTINCT FROM 'string'
        OR octet_length(payload->>'content') NOT BETWEEN 1 AND 65536
        OR jsonb_typeof(payload->'metadata') IS DISTINCT FROM 'object'
        OR NOT knowledge.is_valid_metadata(payload->'metadata')
      )
  ) THEN
    RAISE EXCEPTION 'pending chunk contract is invalid';
  END IF;

  IF EXISTS (
    SELECT payload->>'chunk_id'
    FROM knowledge.ingestion_pending
    WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid
    GROUP BY payload->>'chunk_id'
    HAVING count(*) > 1
  ) THEN
    RAISE EXCEPTION 'pending chunk set contains duplicate identifiers';
  END IF;

  IF (
    SELECT count(DISTINCT payload #>> '{metadata,source,id}')
    FROM knowledge.ingestion_pending
    WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid
  ) <> 1 THEN
    RAISE EXCEPTION 'pending chunk set must contain exactly one source';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_pending AS pending
    JOIN knowledge.chunks AS chunks
      ON chunks.chunk_id = pending.payload->>'chunk_id'
    WHERE pending.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND (
        chunks.content IS DISTINCT FROM pending.payload->>'content'
        OR chunks.metadata #>> '{source,id}'
          IS DISTINCT FROM pending.payload #>> '{metadata,source,id}'
      )
  ) THEN
    RAISE EXCEPTION 'stable chunk identifier collides with different stored content or source';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_pending AS pending
    JOIN knowledge.ingestion_embedding_cache AS cache
      ON cache.profile = 'bge-m3-1024-v1'
      AND cache.source_id = pending.payload #>> '{metadata,source,id}'
      AND cache.content_sha256 = encode(
        sha256(convert_to(pending.payload->>'content', 'UTF8')),
        'hex'
      )
      AND cache.expires_at > clock_timestamp()
    WHERE pending.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND cache.content IS DISTINCT FROM pending.payload->>'content'
  ) THEN
    RAISE EXCEPTION 'embedding cache SHA256 collides with different exact content';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_pending AS pending
    JOIN knowledge.chunks AS left_chunk
      ON left_chunk.content = pending.payload->>'content'
    JOIN knowledge.chunks AS right_chunk
      ON right_chunk.content = pending.payload->>'content'
      AND right_chunk.chunk_id > left_chunk.chunk_id
    WHERE pending.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND left_chunk.embedding::text IS DISTINCT FROM right_chunk.embedding::text
  ) THEN
    RAISE EXCEPTION 'canonical embeddings conflict for exact content';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_pending AS pending
    JOIN knowledge.chunks AS chunks
      ON chunks.content = pending.payload->>'content'
    JOIN knowledge.ingestion_embedding_cache AS cache
      ON cache.profile = 'bge-m3-1024-v1'
      AND cache.source_id = pending.payload #>> '{metadata,source,id}'
      AND cache.content_sha256 = encode(
        sha256(convert_to(pending.payload->>'content', 'UTF8')),
        'hex'
      )
      AND cache.content = pending.payload->>'content'
      AND cache.expires_at > clock_timestamp()
    WHERE pending.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND chunks.embedding::text IS DISTINCT FROM cache.embedding::text
  ) THEN
    RAISE EXCEPTION 'canonical and checkpoint embeddings conflict for exact content';
  END IF;
END
$validation$;

-- Retain the active input's exact receipts first, then the newest unrelated receipts. The active
-- input has at most 512 chunks, so the 1,024-row bound preserves one complete interleaved retry.
WITH active_cache AS (
  SELECT DISTINCT
    payload #>> '{metadata,source,id}' AS source_id,
    encode(sha256(convert_to(payload->>'content', 'UTF8')), 'hex') AS content_sha256,
    payload->>'content' AS content
  FROM knowledge.ingestion_pending
  WHERE run_id = :'run_id'::uuid
),
ranked_cache AS (
  SELECT
    cache.ctid,
    row_number() OVER (
      ORDER BY
        EXISTS (
          SELECT 1
          FROM active_cache
          WHERE active_cache.source_id = cache.source_id
            AND active_cache.content_sha256 = cache.content_sha256
            AND active_cache.content = cache.content
        ) DESC,
        cache.expires_at DESC,
        cache.source_id,
        cache.profile,
        cache.content_sha256
    ) AS retention_rank
  FROM knowledge.ingestion_embedding_cache AS cache
)
DELETE FROM knowledge.ingestion_embedding_cache AS cache
USING ranked_cache
WHERE cache.ctid = ranked_cache.ctid
  AND ranked_cache.retention_rank > 1024;

\pset tuples_only on
\pset format unaligned
\o /work/plan.jsonl
SELECT jsonb_build_object(
  'chunk_id', pending.payload->>'chunk_id',
  'content', pending.payload->>'content',
  'metadata', pending.payload->'metadata',
  'embedding',
    CASE
      WHEN chunks.chunk_id IS NOT NULL
      THEN (chunks.embedding::text)::jsonb
      WHEN canonical_content.embedding IS NOT NULL
      THEN (canonical_content.embedding::text)::jsonb
      WHEN cache.content = pending.payload->>'content'
      THEN (cache.embedding::text)::jsonb
      ELSE NULL
    END
)::text
FROM knowledge.ingestion_pending AS pending
LEFT JOIN knowledge.chunks AS chunks
  ON chunks.chunk_id = pending.payload->>'chunk_id'
LEFT JOIN LATERAL (
  SELECT content_chunk.embedding
  FROM knowledge.chunks AS content_chunk
  WHERE content_chunk.content = pending.payload->>'content'
  ORDER BY content_chunk.chunk_id
  LIMIT 1
) AS canonical_content ON true
LEFT JOIN knowledge.ingestion_embedding_cache AS cache
  ON cache.profile = 'bge-m3-1024-v1'
  AND cache.source_id = pending.payload #>> '{metadata,source,id}'
  AND cache.content_sha256 = encode(
    sha256(convert_to(pending.payload->>'content', 'UTF8')),
    'hex'
  )
  AND cache.expires_at > clock_timestamp()
WHERE pending.run_id = :'run_id'::uuid
ORDER BY pending.payload->>'chunk_id';
\o

COMMIT;
