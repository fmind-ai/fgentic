-- v0 -> v1: Durable delegation ledger and fenced recovery
-- only: postgres until "end only"

-- These two legacy tables predate bridge_version. CREATE IF NOT EXISTS adopts them without
-- rewriting live context or 24-hour event tombstones.
CREATE TABLE IF NOT EXISTS bridge_contexts (
	room_id    TEXT NOT NULL,
	ghost      TEXT NOT NULL,
	context_id TEXT NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (room_id, ghost)
);

CREATE TABLE IF NOT EXISTS bridge_processed_events (
	event_id     TEXT PRIMARY KEY,
	processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS bridge_processed_events_at
	ON bridge_processed_events (processed_at);

CREATE TABLE bridge_appservice_transactions (
	transaction_id TEXT PRIMARY KEY CHECK (transaction_id <> ''),
	body_sha256    BYTEA NOT NULL CHECK (octet_length(body_sha256) = 32),
	committed_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX bridge_appservice_transactions_committed_at
	ON bridge_appservice_transactions (committed_at);

CREATE TABLE bridge_delegations (
	job_id                     TEXT PRIMARY KEY CHECK (job_id <> ''),
	matrix_event_id             TEXT NOT NULL CHECK (matrix_event_id <> ''),
	ghost_mxid                  TEXT NOT NULL CHECK (ghost_mxid <> ''),
	ghost_localpart             TEXT NOT NULL CHECK (ghost_localpart <> ''),
	appservice_transaction_id   TEXT NOT NULL REFERENCES bridge_appservice_transactions(transaction_id),
	room_id                     TEXT NOT NULL CHECK (room_id <> ''),
	intake_sequence             BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
	sender_mxid                 TEXT NOT NULL CHECK (sender_mxid <> ''),
	sender_origin_kind          TEXT NOT NULL DEFAULT '',
	sender_origin_network       TEXT NOT NULL DEFAULT '',
	matrix_origin_server_ts     BIGINT NOT NULL,
	target_fingerprint          TEXT NOT NULL CHECK (target_fingerprint <> ''),
	intake_fingerprint          BYTEA NOT NULL CHECK (octet_length(intake_fingerprint) = 32),
	prompt                      TEXT NOT NULL DEFAULT '',
	payload                     BYTEA NOT NULL DEFAULT '\x',
	state                       TEXT NOT NULL DEFAULT 'pending' CHECK (
		state IN ('pending', 'a2a_prepared', 'awaiting_task', 'reply_pending', 'delivered', 'denied', 'ambiguous', 'dead')
	),
	lease_owner                 TEXT,
	lease_generation            BIGINT NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
	lease_expires_at            TIMESTAMPTZ,
	attempt_count               INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
	poll_count                  INTEGER NOT NULL DEFAULT 0 CHECK (poll_count >= 0),
	next_attempt_at             TIMESTAMPTZ NOT NULL,
	error_code                  TEXT NOT NULL DEFAULT '' CHECK (
		error_code = '' OR (char_length(error_code) <= 128 AND error_code ~ '^[a-z0-9][a-z0-9_.-]*$')
	),
	admission_checked           BOOLEAN NOT NULL DEFAULT false,
	admission_allowed           BOOLEAN NOT NULL DEFAULT false,
	admission_reason            TEXT NOT NULL DEFAULT '' CHECK (
		admission_reason = '' OR (char_length(admission_reason) <= 128 AND admission_reason ~ '^[a-z0-9][a-z0-9_.-]*$')
	),
	a2a_message_id              TEXT NOT NULL CHECK (a2a_message_id <> ''),
	a2a_task_id                 TEXT NOT NULL DEFAULT '',
	a2a_context_id              TEXT NOT NULL DEFAULT '',
	result_text                 TEXT NOT NULL DEFAULT '',
	matrix_reply_txn_id         TEXT NOT NULL CHECK (matrix_reply_txn_id <> ''),
	matrix_placeholder_txn_id   TEXT NOT NULL CHECK (matrix_placeholder_txn_id <> ''),
	matrix_edit_txn_id          TEXT NOT NULL CHECK (matrix_edit_txn_id <> ''),
	matrix_reply_event_id       TEXT NOT NULL DEFAULT '',
	matrix_placeholder_event_id TEXT NOT NULL DEFAULT '',
	matrix_edit_event_id        TEXT NOT NULL DEFAULT '',
	created_at                  TIMESTAMPTZ NOT NULL,
	updated_at                  TIMESTAMPTZ NOT NULL,
	terminal_at                 TIMESTAMPTZ,
	UNIQUE (matrix_event_id, ghost_mxid),
	CHECK (NOT admission_allowed OR admission_checked),
	CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
	CHECK (lease_owner IS NULL OR lease_owner <> ''),
	CHECK (
		(state IN ('delivered', 'denied', 'ambiguous', 'dead')) = (terminal_at IS NOT NULL)
	)
);

CREATE INDEX bridge_delegations_claim
	ON bridge_delegations (next_attempt_at, intake_sequence)
	WHERE terminal_at IS NULL;

CREATE INDEX bridge_delegations_room_fifo
	ON bridge_delegations (room_id, intake_sequence)
	WHERE terminal_at IS NULL;

CREATE INDEX bridge_delegations_lease_expiry
	ON bridge_delegations (lease_expires_at)
	WHERE lease_expires_at IS NOT NULL AND terminal_at IS NULL;

CREATE INDEX bridge_delegations_terminal_cleanup
	ON bridge_delegations (terminal_at)
	WHERE terminal_at IS NOT NULL;

-- end only postgres
