\set ON_ERROR_STOP on
\set VERBOSITY terse

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
SET LOCAL idle_in_transaction_session_timeout = '30s';

-- A dead Job cannot retain an action forever. The 35-minute database ceiling is deliberately
-- longer than the current 30-minute ingestion Job deadline and cannot be extended by replay.
UPDATE knowledge.connector_sources
SET claim_holder = NULL,
    claimed_at = NULL,
    claim_expires_at = NULL
WHERE claim_expires_at <= transaction_timestamp();

\pset tuples_only on
\pset format unaligned
\o /work/connector-action.json
WITH existing AS MATERIALIZED (
  SELECT sources.source_id,
    sources.claim_expires_at
  FROM knowledge.connector_sources AS sources
  WHERE sources.claim_holder = :'run_id'::uuid
    AND sources.claim_expires_at > transaction_timestamp()
  FOR UPDATE
), candidate AS MATERIALIZED (
  SELECT sources.source_id
  FROM knowledge.connector_sources AS sources
  JOIN knowledge.connector_snapshots AS snapshots
    ON snapshots.connector_id = sources.connector_id
    AND snapshots.enumeration_complete
    AND snapshots.blocked_at IS NULL
    AND snapshots.desired_revision = sources.desired_snapshot_revision
    AND snapshots.desired_inventory_digest = sources.desired_inventory_digest
  WHERE NOT EXISTS (SELECT 1 FROM existing)
    AND sources.claim_holder IS NULL
    AND (
      sources.applied_action IS DISTINCT FROM sources.desired_action
      OR sources.applied_revision IS DISTINCT FROM sources.desired_revision
      OR sources.applied_digest IS DISTINCT FROM sources.desired_digest
      OR sources.applied_acl_digest IS DISTINCT FROM sources.desired_acl_digest
      OR sources.applied_metadata IS DISTINCT FROM sources.desired_metadata
      OR sources.applied_snapshot_revision IS DISTINCT FROM sources.desired_snapshot_revision
      OR sources.applied_inventory_digest IS DISTINCT FROM sources.desired_inventory_digest
    )
  ORDER BY sources.connector_id, sources.source_id
  FOR UPDATE OF sources SKIP LOCKED
  LIMIT 1
), claimed AS (
  UPDATE knowledge.connector_sources AS sources
  SET claim_holder = :'run_id'::uuid,
      claimed_at = transaction_timestamp(),
      claim_expires_at = transaction_timestamp() + interval '35 minutes'
  FROM candidate
  WHERE sources.source_id = candidate.source_id
  RETURNING sources.source_id,
    sources.claim_expires_at
), selected AS (
  SELECT source_id, claim_expires_at FROM existing
  UNION ALL
  SELECT source_id, claim_expires_at FROM claimed
)
SELECT jsonb_build_object(
  'connector_id', sources.connector_id,
  'source_id', sources.source_id,
  'source_path', sources.source_path,
  'action', sources.desired_action,
  'source_revision', sources.desired_revision,
  'content_digest', sources.desired_digest,
  'acl_digest', sources.desired_acl_digest,
  'metadata', sources.desired_metadata,
  'snapshot_revision', sources.desired_snapshot_revision,
  'inventory_digest', sources.desired_inventory_digest,
  'claim_expires_at', selected.claim_expires_at
)::text
FROM selected
JOIN knowledge.connector_sources AS sources USING (source_id);
\o

\o /work/connector-kind
SELECT desired_action
FROM knowledge.connector_sources
WHERE claim_holder = :'run_id'::uuid
  AND claim_expires_at > transaction_timestamp();
\o

COMMIT;
