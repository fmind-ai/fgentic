\set ON_ERROR_STOP on
\set VERBOSITY terse

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';
SET LOCAL idle_in_transaction_session_timeout = '60s';

CREATE TEMPORARY TABLE connector_snapshot_input (
  payload jsonb NOT NULL
) ON COMMIT DROP;

\copy connector_snapshot_input (payload) FROM '/sources/.connector/git-markdown/current.json' WITH (FORMAT csv, DELIMITER E'\x1f', QUOTE E'\x1e', ESCAPE E'\x1e')

DO $publish$
DECLARE
  snapshot jsonb;
  source_inventory jsonb;
  canonical_inventory jsonb;
  inventory_digest text;
  current_snapshot knowledge.connector_snapshots%ROWTYPE;
BEGIN
  IF (SELECT count(*) FROM connector_snapshot_input) <> 1 THEN
    RAISE EXCEPTION 'connector snapshot input must contain exactly one JSON object';
  END IF;

  SELECT payload INTO STRICT snapshot
  FROM connector_snapshot_input;

  IF snapshot ? 'blocked' THEN
    IF jsonb_typeof(snapshot) IS DISTINCT FROM 'object'
      OR NOT snapshot ?& ARRAY[
        'connector_id',
        'blocked',
        'snapshot_revision',
        'artifact_digest',
        'reason'
      ]
      OR snapshot - ARRAY[
        'connector_id',
        'blocked',
        'snapshot_revision',
        'artifact_digest',
        'reason'
      ] <> '{}'::jsonb
      OR snapshot->>'connector_id' IS DISTINCT FROM 'git-markdown'
      OR jsonb_typeof(snapshot->'blocked') IS DISTINCT FROM 'boolean'
      OR snapshot->>'blocked' IS DISTINCT FROM 'true'
      OR jsonb_typeof(snapshot->'snapshot_revision') IS DISTINCT FROM 'string'
      OR jsonb_typeof(snapshot->'artifact_digest') IS DISTINCT FROM 'string'
      OR (snapshot->>'artifact_digest') !~ '^sha256:[0-9a-f]{64}$'
      OR snapshot->>'reason' IS DISTINCT FROM 'artifact-rejected'
    THEN
      RAISE EXCEPTION 'blocked connector snapshot envelope is invalid';
    END IF;
    PERFORM knowledge.block_connector_snapshot(
      snapshot->>'connector_id',
      snapshot->>'snapshot_revision',
      snapshot->>'artifact_digest'
    );
    RETURN;
  END IF;

  IF jsonb_typeof(snapshot) IS DISTINCT FROM 'object'
    OR NOT snapshot ?& ARRAY[
      'connector_id',
      'snapshot_revision',
      'artifact_digest',
      'inventory_digest',
      'source_count',
      'sources'
    ]
    OR snapshot - ARRAY[
      'connector_id',
      'snapshot_revision',
      'artifact_digest',
      'inventory_digest',
      'source_count',
      'sources'
    ] <> '{}'::jsonb
    OR jsonb_typeof(snapshot->'connector_id') IS DISTINCT FROM 'string'
    OR jsonb_typeof(snapshot->'snapshot_revision') IS DISTINCT FROM 'string'
    OR jsonb_typeof(snapshot->'artifact_digest') IS DISTINCT FROM 'string'
    OR (snapshot->>'artifact_digest') !~ '^sha256:[0-9a-f]{64}$'
    OR jsonb_typeof(snapshot->'inventory_digest') IS DISTINCT FROM 'string'
    OR (snapshot->>'inventory_digest') !~ '^sha256:[0-9a-f]{64}$'
    OR jsonb_typeof(snapshot->'source_count') IS DISTINCT FROM 'number'
    OR (snapshot->>'source_count') !~ '^(0|[1-9][0-9]{0,2})$'
    OR (snapshot->>'source_count')::integer NOT BETWEEN 0 AND 512
    OR jsonb_typeof(snapshot->'sources') IS DISTINCT FROM 'array'
    OR jsonb_array_length(snapshot->'sources') <> (snapshot->>'source_count')::integer
  THEN
    RAISE EXCEPTION 'connector snapshot envelope is invalid';
  END IF;

  source_inventory := snapshot->'sources';

  IF EXISTS (
    SELECT 1
    FROM jsonb_array_elements(source_inventory) AS source(item)
    WHERE jsonb_typeof(source.item) IS DISTINCT FROM 'object'
      OR NOT source.item ?& ARRAY[
        'connector_id',
        'snapshot_revision',
        'inventory_digest',
        'source_id',
        'source_path',
        'source_revision',
        'content_digest',
        'acl_digest',
        'metadata'
      ]
      OR source.item - ARRAY[
        'connector_id',
        'snapshot_revision',
        'inventory_digest',
        'source_id',
        'source_path',
        'source_revision',
        'content_digest',
        'acl_digest',
        'metadata'
      ] <> '{}'::jsonb
      OR source.item->>'connector_id' IS DISTINCT FROM snapshot->>'connector_id'
      OR source.item->>'snapshot_revision' IS DISTINCT FROM snapshot->>'snapshot_revision'
      OR source.item->>'inventory_digest' IS DISTINCT FROM snapshot->>'inventory_digest'
      OR jsonb_typeof(source.item->'source_id') IS DISTINCT FROM 'string'
      OR jsonb_typeof(source.item->'source_path') IS DISTINCT FROM 'string'
      OR jsonb_typeof(source.item->'source_revision') IS DISTINCT FROM 'string'
      OR jsonb_typeof(source.item->'content_digest') IS DISTINCT FROM 'string'
      OR jsonb_typeof(source.item->'acl_digest') IS DISTINCT FROM 'string'
      OR jsonb_typeof(source.item->'metadata') IS DISTINCT FROM 'object'
  ) THEN
    RAISE EXCEPTION 'connector snapshot source inventory is invalid';
  END IF;

  -- The envelope repeats snapshot binding fields on every source so the retained file can be
  -- materialized independently. They are not part of the connector's canonical inventory digest.
  SELECT COALESCE(
    jsonb_agg(
      jsonb_build_object(
        'source_id', source.item->>'source_id',
        'source_path', source.item->>'source_path',
        'source_revision', source.item->>'source_revision',
        'content_digest', source.item->>'content_digest',
        'acl_digest', source.item->>'acl_digest',
        'metadata', source.item->'metadata'
      ) ORDER BY (source.item->>'source_id') COLLATE "C"
    ),
    '[]'::jsonb
  )
  INTO canonical_inventory
  FROM jsonb_array_elements(source_inventory) AS source(item);

  inventory_digest := knowledge.connector_inventory_json_digest(canonical_inventory);
  IF inventory_digest IS DISTINCT FROM snapshot->>'inventory_digest' THEN
    RAISE EXCEPTION 'connector snapshot canonical inventory digest does not match';
  END IF;

  SELECT * INTO current_snapshot
  FROM knowledge.connector_snapshots
  WHERE connector_id = snapshot->>'connector_id';

  -- Replaying the current complete desired snapshot is a no-op and preserves any active claim.
  -- A different complete inventory preempts older pending work below so ACL revocation and source
  -- deletion cannot wait behind a stale repository cursor.
  IF FOUND AND current_snapshot.enumeration_complete THEN
    IF (
      current_snapshot.desired_revision,
      current_snapshot.desired_inventory_digest
    ) = (
      snapshot->>'snapshot_revision',
      inventory_digest
    ) AND current_snapshot.blocked_at IS NULL THEN
      RETURN;
    END IF;
  END IF;

  PERFORM knowledge.begin_connector_snapshot(
    snapshot->>'connector_id',
    snapshot->>'snapshot_revision',
    inventory_digest,
    (snapshot->>'source_count')::integer
  );

  INSERT INTO knowledge.connector_inventory (
    connector_id,
    snapshot_revision,
    inventory_digest,
    source_id,
    source_path,
    source_revision,
    content_digest,
    acl_digest,
    metadata
  )
  SELECT
    source.item->>'connector_id',
    source.item->>'snapshot_revision',
    source.item->>'inventory_digest',
    source.item->>'source_id',
    source.item->>'source_path',
    source.item->>'source_revision',
    source.item->>'content_digest',
    source.item->>'acl_digest',
    source.item->'metadata'
  FROM jsonb_array_elements(source_inventory) AS source(item)
  ORDER BY source.item->>'source_id';

  PERFORM knowledge.complete_connector_snapshot(
    snapshot->>'connector_id',
    snapshot->>'snapshot_revision',
    inventory_digest
  );
END
$publish$;

COMMIT;
