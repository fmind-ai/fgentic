\set ON_ERROR_STOP on
\set VERBOSITY terse

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '120s';
SET LOCAL idle_in_transaction_session_timeout = '60s';

SELECT set_config('fgentic.ingestion_run_id', :'run_id', false);

DO $lease$
BEGIN
  PERFORM 1
  FROM knowledge.ingestion_leases
  WHERE name = 'chunks-v1'
    AND holder = current_setting('fgentic.ingestion_run_id')::uuid
    AND expires_at > clock_timestamp()
  FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'knowledge ingestion lease is absent, expired, or owned by another run';
  END IF;
END
$lease$;

DELETE FROM knowledge.ingestion_final
WHERE run_id = :'run_id'::uuid;

\copy knowledge.ingestion_final (payload) FROM '/work/chunks.jsonl' WITH (FORMAT csv, DELIMITER E'\x1f', QUOTE E'\x1e', ESCAPE E'\x1e')

DO $validation$
DECLARE
  staged_count integer;
BEGIN
  SELECT count(*) INTO staged_count
  FROM knowledge.ingestion_final
  WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid;

  IF staged_count NOT BETWEEN 1 AND 512 THEN
    RAISE EXCEPTION 'final chunk set must contain between 1 and 512 rows';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final
    WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND (
        jsonb_typeof(payload) IS DISTINCT FROM 'object'
        OR NOT payload ?& ARRAY['chunk_id', 'content', 'metadata', 'embedding']
        OR payload - ARRAY['chunk_id', 'content', 'metadata', 'embedding'] <> '{}'::jsonb
        OR jsonb_typeof(payload->'chunk_id') IS DISTINCT FROM 'string'
        OR (payload->>'chunk_id') !~ '^sha256:[0-9a-f]{64}$'
        OR jsonb_typeof(payload->'content') IS DISTINCT FROM 'string'
        OR octet_length(payload->>'content') NOT BETWEEN 1 AND 65536
        OR jsonb_typeof(payload->'metadata') IS DISTINCT FROM 'object'
        OR NOT knowledge.is_valid_metadata(payload->'metadata')
        OR jsonb_typeof(payload->'embedding') IS DISTINCT FROM 'array'
        OR jsonb_array_length(payload->'embedding') <> 1024
      )
  ) THEN
    RAISE EXCEPTION 'final chunk contract is invalid';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS final
    CROSS JOIN LATERAL jsonb_array_elements(final.payload->'embedding') AS values(item)
    WHERE final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND jsonb_typeof(values.item) IS DISTINCT FROM 'number'
  ) THEN
    RAISE EXCEPTION 'final embedding contains a non-number';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final
    WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND vector_norm((payload->'embedding')::text::vector(1024)) <= 0
  ) THEN
    RAISE EXCEPTION 'final embedding must remain non-zero in pgvector float32';
  END IF;

  IF EXISTS (
    SELECT payload->>'chunk_id'
    FROM knowledge.ingestion_final
    WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid
    GROUP BY payload->>'chunk_id'
    HAVING count(*) > 1
  ) THEN
    RAISE EXCEPTION 'final chunk set contains duplicate identifiers';
  END IF;

  IF (
    SELECT count(DISTINCT payload #>> '{metadata,source,id}')
    FROM knowledge.ingestion_final
    WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid
  ) <> 1 THEN
    RAISE EXCEPTION 'final chunk set must contain exactly one source';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_pending AS pending
    FULL JOIN knowledge.ingestion_final AS final
      ON final.run_id = pending.run_id
      AND final.payload->>'chunk_id' = pending.payload->>'chunk_id'
    WHERE COALESCE(pending.run_id, final.run_id)
        = current_setting('fgentic.ingestion_run_id')::uuid
      AND (
        pending.run_id IS NULL
        OR final.run_id IS NULL
        OR pending.payload->>'content' IS DISTINCT FROM final.payload->>'content'
        OR pending.payload->'metadata' IS DISTINCT FROM final.payload->'metadata'
      )
  ) THEN
    RAISE EXCEPTION 'embedding phase changed the authoritative bound chunk set';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS final
    JOIN knowledge.chunks AS chunks
      ON chunks.chunk_id = final.payload->>'chunk_id'
    WHERE final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND (
        chunks.content IS DISTINCT FROM final.payload->>'content'
        OR chunks.metadata #>> '{source,id}'
          IS DISTINCT FROM final.payload #>> '{metadata,source,id}'
      )
  ) THEN
    RAISE EXCEPTION 'stable chunk identifier collides with different stored content or source';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS final
    JOIN knowledge.chunks AS left_chunk
      ON left_chunk.content = final.payload->>'content'
    JOIN knowledge.chunks AS right_chunk
      ON right_chunk.content = final.payload->>'content'
      AND right_chunk.chunk_id > left_chunk.chunk_id
    WHERE final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND left_chunk.embedding::text IS DISTINCT FROM right_chunk.embedding::text
  ) THEN
    RAISE EXCEPTION 'canonical embeddings conflict for exact content';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS final
    JOIN knowledge.ingestion_embedding_cache AS cache
      ON cache.profile = 'bge-m3-1024-v1'
      AND cache.source_id = final.payload #>> '{metadata,source,id}'
      AND cache.content_sha256 = encode(
        sha256(convert_to(final.payload->>'content', 'UTF8')),
        'hex'
      )
      AND cache.expires_at > clock_timestamp()
    WHERE final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND cache.content IS DISTINCT FROM final.payload->>'content'
  ) THEN
    RAISE EXCEPTION 'embedding cache SHA256 collides with different exact content';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS final
    JOIN knowledge.chunks AS chunks
      ON chunks.content = final.payload->>'content'
    JOIN knowledge.ingestion_embedding_cache AS cache
      ON cache.profile = 'bge-m3-1024-v1'
      AND cache.source_id = final.payload #>> '{metadata,source,id}'
      AND cache.content_sha256 = encode(
        sha256(convert_to(final.payload->>'content', 'UTF8')),
        'hex'
      )
      AND cache.content = final.payload->>'content'
      AND cache.expires_at > clock_timestamp()
    WHERE final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND chunks.embedding::text IS DISTINCT FROM cache.embedding::text
  ) THEN
    RAISE EXCEPTION 'canonical and checkpoint embeddings conflict for exact content';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS final
    WHERE final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND NOT EXISTS (
        SELECT 1
        FROM knowledge.chunks AS chunks
        WHERE chunks.content = final.payload->>'content'
          AND chunks.embedding::text =
            ((final.payload->'embedding')::text::vector(1024))::text
      )
      AND NOT EXISTS (
        SELECT 1
        FROM knowledge.ingestion_embedding_cache AS cache
        WHERE cache.profile = 'bge-m3-1024-v1'
          AND cache.source_id = final.payload #>> '{metadata,source,id}'
          AND cache.content_sha256 = encode(
            sha256(convert_to(final.payload->>'content', 'UTF8')),
            'hex'
          )
          AND cache.content = final.payload->>'content'
          AND cache.expires_at > clock_timestamp()
          AND cache.embedding::text =
            ((final.payload->'embedding')::text::vector(1024))::text
      )
  ) THEN
    RAISE EXCEPTION 'final embedding lacks canonical or checkpoint provenance';
  END IF;
END
$validation$;

INSERT INTO knowledge.chunks (chunk_id, content, embedding, metadata)
SELECT
  payload->>'chunk_id',
  payload->>'content',
  (payload->'embedding')::text::vector(1024),
  payload->'metadata'
FROM knowledge.ingestion_final
WHERE run_id = :'run_id'::uuid
ON CONFLICT (chunk_id) DO UPDATE
SET
  content = EXCLUDED.content,
  embedding = EXCLUDED.embedding,
  metadata = EXCLUDED.metadata
WHERE (
  knowledge.chunks.content,
  knowledge.chunks.embedding,
  knowledge.chunks.metadata
) IS DISTINCT FROM (
  EXCLUDED.content,
  EXCLUDED.embedding,
  EXCLUDED.metadata
);

WITH ingested_sources AS (
  SELECT DISTINCT payload #>> '{metadata,source,id}' AS source_id
  FROM knowledge.ingestion_pending
  WHERE run_id = :'run_id'::uuid
)
DELETE FROM knowledge.chunks AS chunks
USING ingested_sources AS sources
WHERE chunks.metadata #>> '{source,id}' = sources.source_id
  AND NOT EXISTS (
    SELECT 1
    FROM knowledge.ingestion_pending AS pending
    WHERE pending.run_id = :'run_id'::uuid
      AND pending.payload->>'chunk_id' = chunks.chunk_id
  );

WITH committed_sources AS (
  SELECT DISTINCT payload #>> '{metadata,source,id}' AS source_id
  FROM knowledge.ingestion_final
  WHERE run_id = :'run_id'::uuid
)
DELETE FROM knowledge.ingestion_embedding_cache AS cache
USING committed_sources
WHERE cache.source_id = committed_sources.source_id;

-- Connector runs opt in explicitly. Completing the source here keeps the chunk replacement,
-- source checkpoint, and repository cursor in this same transaction; ordinary #332 runs omit the
-- variable and retain their original behavior.
\if :{?connector_action}
  SELECT knowledge.complete_connector_present(:'run_id'::uuid);
\endif

DELETE FROM knowledge.ingestion_pending
WHERE run_id = :'run_id'::uuid;
DELETE FROM knowledge.ingestion_final
WHERE run_id = :'run_id'::uuid;
DELETE FROM knowledge.ingestion_leases
WHERE name = 'chunks-v1'
  AND holder = :'run_id'::uuid;

COMMIT;
