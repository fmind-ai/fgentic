\set ON_ERROR_STOP on
\set VERBOSITY terse

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '120s';
SET LOCAL idle_in_transaction_session_timeout = '60s';

-- Tombstones serialize with normal ingestion. Deletion, connector source advancement, and the
-- complete repository cursor either commit together or remain entirely unapplied.
DELETE FROM knowledge.ingestion_leases
WHERE expires_at <= transaction_timestamp();
INSERT INTO knowledge.ingestion_leases (name, holder, expires_at)
VALUES (
  'chunks-v1',
  :'run_id'::uuid,
  transaction_timestamp() + interval '35 minutes'
);

SELECT knowledge.apply_connector_tombstone(:'run_id'::uuid);

DELETE FROM knowledge.ingestion_leases
WHERE name = 'chunks-v1'
  AND holder = :'run_id'::uuid;

COMMIT;
