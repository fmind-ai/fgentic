-- v3: persist the homeserver delay ID used by the long-task dead-man switch. It is operational
-- identity only (no room content) and remains on terminal tombstones as cancellation evidence.
ALTER TABLE bridge_delegations
	ADD COLUMN matrix_dead_man_delay_id TEXT NOT NULL DEFAULT '';
