\set ON_ERROR_STOP on
\set VERBOSITY terse

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
SET LOCAL idle_in_transaction_session_timeout = '30s';

-- The cache is capped at 1,024 rows by the single-writer ingestion path. Keep the explicit limit
-- so this independent collector remains bounded even while recovering an older oversized table.
WITH expired AS (
  SELECT
    ctid
  FROM knowledge.ingestion_embedding_cache
  WHERE expires_at <= clock_timestamp()
  ORDER BY
    expires_at,
    source_id,
    profile,
    content_sha256
  LIMIT 1024
)
DELETE FROM knowledge.ingestion_embedding_cache AS cache
USING expired
WHERE cache.ctid = expired.ctid;

COMMIT;
