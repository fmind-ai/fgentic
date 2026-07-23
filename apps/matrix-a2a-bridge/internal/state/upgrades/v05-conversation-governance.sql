-- v4 -> v5: retain the bounded backend-session owners needed for verified conversation purge.
-- only: postgres until "end only"

ALTER TABLE bridge_contexts
	ADD COLUMN owners JSONB NOT NULL DEFAULT '[]'::jsonb,
	ADD COLUMN owners_complete BOOLEAN NOT NULL DEFAULT false,
	ADD CONSTRAINT bridge_contexts_owners_array CHECK (jsonb_typeof(owners) = 'array'),
	ADD CONSTRAINT bridge_contexts_owners_bound CHECK (jsonb_array_length(owners) <= 256);

CREATE INDEX bridge_contexts_retention
	ON bridge_contexts (ghost, updated_at)
	WHERE owners_complete;

-- Existing pre-governance rows deliberately retain an incomplete owner set. Recording later
-- invokers cannot reconstruct historical identities, so those rows remain fail-closed until the
-- context is replaced after an operator-managed backend cleanup.

-- end only postgres
