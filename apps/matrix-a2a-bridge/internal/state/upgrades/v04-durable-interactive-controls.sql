-- v3 -> v4: Durable interactive controls and paused-task clocks
-- only: postgres until "end only"

ALTER TABLE bridge_delegations
	DROP CONSTRAINT bridge_delegations_state_check;

ALTER TABLE bridge_delegations
	ADD CONSTRAINT bridge_delegations_state_check CHECK (
		state IN (
			'pending', 'a2a_prepared', 'awaiting_task', 'awaiting_input',
			'reply_pending', 'delivered', 'denied', 'ambiguous', 'dead'
		)
	),
	ADD COLUMN task_deadline_at TIMESTAMPTZ,
	ADD COLUMN input_wait_started_at TIMESTAMPTZ,
	ADD COLUMN input_wait_expires_at TIMESTAMPTZ,
	ADD CONSTRAINT bridge_delegations_input_wait_check CHECK (
		(input_wait_started_at IS NULL) = (input_wait_expires_at IS NULL)
	),
	ADD CONSTRAINT bridge_delegations_input_state_check CHECK (
		(state = 'awaiting_input') = (input_wait_started_at IS NOT NULL)
	);

CREATE TABLE bridge_delegation_controls (
	control_id                    TEXT PRIMARY KEY CHECK (control_id <> ''),
	job_id                        TEXT NOT NULL REFERENCES bridge_delegations(job_id) ON DELETE CASCADE,
	appservice_transaction_id     TEXT NOT NULL REFERENCES bridge_appservice_transactions(transaction_id),
	source_matrix_event_id        TEXT NOT NULL DEFAULT '',
	intake_fingerprint            BYTEA NOT NULL DEFAULT '\x' CHECK (octet_length(intake_fingerprint) IN (0, 32)),
	authorized_sender             TEXT NOT NULL CHECK (authorized_sender <> ''),
	kind                          TEXT NOT NULL CHECK (
		kind IN ('cancel', 'continuation', 'question', 'progress', 'pin', 'unpin')
	),
	state                         TEXT NOT NULL DEFAULT 'pending' CHECK (
		state IN ('pending', 'prepared', 'applied', 'ambiguous', 'denied', 'dead')
	),
	slot                          INTEGER NOT NULL DEFAULT 0 CHECK (slot >= 0),
	lease_generation              BIGINT NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
	recovery_count                INTEGER NOT NULL DEFAULT 0 CHECK (recovery_count >= 0),
	payload                       BYTEA NOT NULL DEFAULT '\x',
	a2a_message_id                TEXT NOT NULL CHECK (a2a_message_id <> ''),
	matrix_txn_id                 TEXT NOT NULL CHECK (matrix_txn_id <> ''),
	matrix_event_id               TEXT NOT NULL DEFAULT '',
	error_code                    TEXT NOT NULL DEFAULT '' CHECK (
		error_code = '' OR (char_length(error_code) <= 128 AND error_code ~ '^[a-z0-9][a-z0-9_.-]*$')
	),
	prepared_at                   TIMESTAMPTZ,
	created_at                    TIMESTAMPTZ NOT NULL,
	updated_at                    TIMESTAMPTZ NOT NULL,
	terminal_at                   TIMESTAMPTZ,
	UNIQUE (job_id, kind, source_matrix_event_id, slot),
	CHECK ((state IN ('applied', 'ambiguous', 'denied', 'dead')) = (terminal_at IS NOT NULL))
);

CREATE INDEX bridge_delegation_controls_pending
	ON bridge_delegation_controls (job_id, created_at, control_id)
	WHERE terminal_at IS NULL;

CREATE INDEX bridge_delegation_controls_terminal
	ON bridge_delegation_controls (terminal_at)
	WHERE terminal_at IS NOT NULL;

-- end only postgres
