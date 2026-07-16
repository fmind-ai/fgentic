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

DELETE FROM knowledge.ingestion_embedding_cache
WHERE expires_at <= clock_timestamp();

DELETE FROM knowledge.ingestion_final
WHERE run_id = :'run_id'::uuid;

-- PSTDIN deliberately reads the embedding output piped to psql even though this SQL is loaded
-- through --file. The transaction leaves neither partial staging nor partial cache entries.
\copy knowledge.ingestion_final (payload) FROM PSTDIN WITH (FORMAT csv, DELIMITER E'\x1f', QUOTE E'\x1e', ESCAPE E'\x1e')

DO $validation$
DECLARE
  staged_count integer;
BEGIN
  SELECT count(*) INTO staged_count
  FROM knowledge.ingestion_final
  WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid;

  IF staged_count NOT BETWEEN 1 AND 8 THEN
    RAISE EXCEPTION 'embedding checkpoint must contain between 1 and 8 rows';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final
    WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND (
        jsonb_typeof(payload) IS DISTINCT FROM 'object'
        OR NOT payload ?& ARRAY['profile', 'content', 'embedding']
        OR payload - ARRAY['profile', 'content', 'embedding'] <> '{}'::jsonb
        OR jsonb_typeof(payload->'profile') IS DISTINCT FROM 'string'
        OR payload->>'profile' <> 'bge-m3-1024-v1'
        OR jsonb_typeof(payload->'content') IS DISTINCT FROM 'string'
        OR octet_length(payload->>'content') NOT BETWEEN 1 AND 65536
        OR (payload->>'content') !~ '[^[:space:]]'
        OR jsonb_typeof(payload->'embedding') IS DISTINCT FROM 'array'
        OR jsonb_array_length(payload->'embedding') <> 1024
      )
  ) THEN
    RAISE EXCEPTION 'embedding checkpoint contract is invalid';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS final
    CROSS JOIN LATERAL jsonb_array_elements(final.payload->'embedding') AS values(item)
    WHERE final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND jsonb_typeof(values.item) IS DISTINCT FROM 'number'
  ) THEN
    RAISE EXCEPTION 'embedding checkpoint contains a non-number';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final
    WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND vector_norm((payload->'embedding')::text::vector(1024)) <= 0
  ) THEN
    RAISE EXCEPTION 'checkpoint embedding must remain non-zero in pgvector float32';
  END IF;

  IF EXISTS (
    SELECT payload->>'content'
    FROM knowledge.ingestion_final
    WHERE run_id = current_setting('fgentic.ingestion_run_id')::uuid
    GROUP BY payload->>'content'
    HAVING count(*) > 1
  ) THEN
    RAISE EXCEPTION 'embedding checkpoint contains duplicate semantic inputs';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS final
    WHERE final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND NOT EXISTS (
        SELECT 1
        FROM knowledge.ingestion_pending AS pending
        WHERE pending.run_id = final.run_id
          AND pending.payload->>'content' = final.payload->>'content'
      )
  ) THEN
    RAISE EXCEPTION 'embedding checkpoint content is absent from authoritative pending input';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS left_final
    JOIN knowledge.ingestion_final AS right_final
      ON right_final.run_id = left_final.run_id
      AND right_final.ctid > left_final.ctid
      AND encode(
        sha256(convert_to(right_final.payload->>'content', 'UTF8')),
        'hex'
      ) = encode(
        sha256(convert_to(left_final.payload->>'content', 'UTF8')),
        'hex'
      )
    WHERE left_final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND left_final.payload->>'content'
        IS DISTINCT FROM right_final.payload->>'content'
  ) THEN
    RAISE EXCEPTION 'embedding checkpoint SHA256 collides within the final chunk set';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS left_final
    JOIN knowledge.ingestion_final AS right_final
      ON right_final.run_id = left_final.run_id
      AND right_final.ctid > left_final.ctid
      AND right_final.payload->>'content' = left_final.payload->>'content'
    WHERE left_final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND ((left_final.payload->'embedding')::text::vector(1024))::text
        IS DISTINCT FROM
        ((right_final.payload->'embedding')::text::vector(1024))::text
  ) THEN
    RAISE EXCEPTION 'exact content produced conflicting embeddings in one checkpoint';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS final
    JOIN knowledge.chunks AS chunks
      ON chunks.content = final.payload->>'content'
    WHERE final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND chunks.embedding::text IS DISTINCT FROM
        ((final.payload->'embedding')::text::vector(1024))::text
  ) THEN
    RAISE EXCEPTION 'checkpoint embedding conflicts with canonical exact content';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS final
    JOIN knowledge.ingestion_pending AS pending
      ON pending.run_id = final.run_id
      AND pending.payload->>'content' = final.payload->>'content'
    JOIN knowledge.ingestion_embedding_cache AS cache
      ON cache.profile = final.payload->>'profile'
      AND cache.source_id = pending.payload #>> '{metadata,source,id}'
      AND cache.content_sha256 = encode(
        sha256(convert_to(final.payload->>'content', 'UTF8')),
        'hex'
      )
    WHERE final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND (
        cache.content IS DISTINCT FROM final.payload->>'content'
        OR cache.embedding::text IS DISTINCT FROM
          ((final.payload->'embedding')::text::vector(1024))::text
      )
  ) THEN
    RAISE EXCEPTION 'embedding checkpoint conflicts with cached exact content or vector';
  END IF;
END
$validation$;

-- Refresh only the active source's exact validated rows. The delete and replacement stay in the
-- same transaction, so a crash cannot expose a gap or extend an unrelated source's receipt.
DELETE FROM knowledge.ingestion_embedding_cache AS cache
USING knowledge.ingestion_final AS final, knowledge.ingestion_pending AS pending
WHERE final.run_id = :'run_id'::uuid
  AND pending.run_id = final.run_id
  AND pending.payload->>'content' = final.payload->>'content'
  AND cache.profile = final.payload->>'profile'
  AND cache.source_id = pending.payload #>> '{metadata,source,id}'
  AND cache.content_sha256 = encode(
    sha256(convert_to(final.payload->>'content', 'UTF8')),
    'hex'
  );

WITH checkpoint_rows AS (
  SELECT DISTINCT ON (
    final.payload->>'profile',
    pending.payload #>> '{metadata,source,id}',
    encode(sha256(convert_to(final.payload->>'content', 'UTF8')), 'hex')
  )
    final.payload->>'profile' AS profile,
    pending.payload #>> '{metadata,source,id}' AS source_id,
    encode(
      sha256(convert_to(final.payload->>'content', 'UTF8')),
      'hex'
    ) AS content_sha256,
    final.payload->>'content' AS content,
    (final.payload->'embedding')::text::vector(1024) AS embedding
  FROM knowledge.ingestion_final AS final
  JOIN knowledge.ingestion_pending AS pending
    ON pending.run_id = final.run_id
    AND pending.payload->>'content' = final.payload->>'content'
  WHERE final.run_id = :'run_id'::uuid
  ORDER BY
    final.payload->>'profile',
    pending.payload #>> '{metadata,source,id}',
    encode(sha256(convert_to(final.payload->>'content', 'UTF8')), 'hex')
),
stamped AS (
  SELECT clock_timestamp() AS cached_at
)
INSERT INTO knowledge.ingestion_embedding_cache (
  profile,
  source_id,
  content_sha256,
  content,
  embedding,
  cached_at,
  expires_at
)
SELECT
  checkpoint_rows.profile,
  checkpoint_rows.source_id,
  checkpoint_rows.content_sha256,
  checkpoint_rows.content,
  checkpoint_rows.embedding,
  stamped.cached_at,
  stamped.cached_at + interval '24 hours'
FROM checkpoint_rows
CROSS JOIN stamped
ON CONFLICT (profile, source_id, content_sha256) DO NOTHING;

DO $cache_validation$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM knowledge.ingestion_final AS final
    JOIN knowledge.ingestion_pending AS pending
      ON pending.run_id = final.run_id
      AND pending.payload->>'content' = final.payload->>'content'
    JOIN knowledge.ingestion_embedding_cache AS cache
      ON cache.profile = final.payload->>'profile'
      AND cache.source_id = pending.payload #>> '{metadata,source,id}'
      AND cache.content_sha256 = encode(
        sha256(convert_to(final.payload->>'content', 'UTF8')),
        'hex'
      )
      AND cache.expires_at > clock_timestamp()
    WHERE final.run_id = current_setting('fgentic.ingestion_run_id')::uuid
      AND (
        cache.content IS DISTINCT FROM final.payload->>'content'
        OR cache.embedding::text IS DISTINCT FROM
          ((final.payload->'embedding')::text::vector(1024))::text
      )
  ) THEN
    RAISE EXCEPTION 'embedding checkpoint conflicts with cached exact content or vector';
  END IF;
END
$cache_validation$;

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

DELETE FROM knowledge.ingestion_final
WHERE run_id = :'run_id'::uuid;

COMMIT;
