package activitystate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	// All gateway replicas cooperate on queue admission through this transaction-scoped Postgres
	// advisory lock. The chart remains single-replica, but the storage invariant does not depend on it.
	queueAdmissionLock int64 = 0x4150494e4258
	schema                   = `
CREATE TABLE IF NOT EXISTS activitypub_inbox_activities (
    activity_id TEXT PRIMARY KEY,
    route TEXT NOT NULL CHECK (route IN ('agent', 'group')),
    target TEXT NOT NULL,
    actor_uri TEXT NOT NULL,
    body BYTEA NOT NULL CHECK (octet_length(body) <= 1048576),
    body_hash BYTEA NOT NULL CHECK (octet_length(body_hash) = 32),
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'running', 'succeeded', 'denied', 'failed', 'ignored')),
    status_token TEXT NOT NULL UNIQUE CHECK (length(status_token) = 32),
    location TEXT NOT NULL DEFAULT '',
    result BYTEA NOT NULL DEFAULT '' CHECK (octet_length(result) <= 1048576),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS activitypub_inbox_pending_idx
    ON activitypub_inbox_activities (created_at, activity_id)
    WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS activitypub_inbox_retention_idx
    ON activitypub_inbox_activities (updated_at)
    WHERE state IN ('succeeded', 'denied', 'failed', 'ignored');
CREATE UNIQUE INDEX IF NOT EXISTS activitypub_inbox_result_location_idx
    ON activitypub_inbox_activities (location)
    WHERE location <> '';`
)

// Postgres persists accepted inbox work, opaque status capabilities, and cached outcomes.
type Postgres struct {
	db        *sql.DB
	retention time.Duration
	capacity  int
}

// OpenPostgres opens, verifies, and initializes a Postgres activity ledger.
func OpenPostgres(ctx context.Context, databaseURL string, retention time.Duration, capacity int) (*Postgres, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open activity state database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping activity state database: %w", err)
	}
	store, err := NewPostgres(ctx, db, retention, capacity)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// NewPostgres initializes an existing database handle. The returned Store owns db.
func NewPostgres(ctx context.Context, db *sql.DB, retention time.Duration, capacity int) (*Postgres, error) {
	if db == nil {
		return nil, errors.New("activity state: database is required")
	}
	if retention <= 0 || capacity < 1 {
		return nil, errors.New("activity state: positive retention and queue capacity are required")
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("initialize activity state schema: %w", err)
	}
	return &Postgres{db: db, retention: retention, capacity: capacity}, nil
}

// Enqueue atomically inserts one globally unique pending activity ID or returns its cached outcome.
func (p *Postgres) Enqueue(ctx context.Context, job Job) (Record, bool, error) {
	return p.insert(ctx, job, StatePending)
}

// Ignore atomically inserts terminal ignored work, so it can never be claimed between intake
// validation and the no-delegation decision.
func (p *Postgres) Ignore(ctx context.Context, job Job) (Record, bool, error) {
	return p.insert(ctx, job, StateIgnored)
}

func (p *Postgres) insert(ctx context.Context, job Job, initial State) (Record, bool, error) {
	if err := ValidateJob(job); err != nil {
		return Record{}, false, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, fmt.Errorf("begin activity admission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, queueAdmissionLock); err != nil {
		return Record{}, false, fmt.Errorf("lock activity admission: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
        DELETE FROM activitypub_inbox_activities
        WHERE state IN ('succeeded', 'denied', 'failed', 'ignored')
		  AND updated_at < now() - ($1 * interval '1 second')`, p.retention.Seconds()); err != nil {
		return Record{}, false, fmt.Errorf("prune activity outcomes: %w", err)
	}

	record, err := loadRecord(ctx, tx, `activity_id = $1`, job.ActivityID)
	if err == nil {
		if !sameRecordJob(record, job) {
			return Record{}, false, ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return Record{}, false, fmt.Errorf("commit duplicate activity lookup: %w", err)
		}
		return record, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Record{}, false, err
	}
	if initial == StatePending {
		var queued int
		if err := tx.QueryRowContext(ctx, `
            SELECT count(*) FROM activitypub_inbox_activities
            WHERE state IN ('pending', 'running')`).Scan(&queued); err != nil {
			return Record{}, false, fmt.Errorf("count pending activities: %w", err)
		}
		if queued >= p.capacity {
			return Record{}, false, ErrCapacity
		}
	}
	token, err := newStatusToken()
	if err != nil {
		return Record{}, false, err
	}
	storedBody := job.Body
	if terminal(initial) {
		storedBody = []byte{}
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO activitypub_inbox_activities
            (activity_id, route, target, actor_uri, body, body_hash, state, status_token)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		job.ActivityID, job.Route, job.Target, job.ActorURI, storedBody, bodyHash(job.Body), initial, token); err != nil {
		return Record{}, false, fmt.Errorf("insert activity: %w", err)
	}
	record, err = loadRecord(ctx, tx, `activity_id = $1`, job.ActivityID)
	if err != nil {
		return Record{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, false, fmt.Errorf("commit activity admission: %w", err)
	}
	return record, true, nil
}

// Claim atomically takes the oldest pending activity. SKIP LOCKED keeps the invariant correct if a
// future deployment adds processors, although the v1 chart deliberately runs one replica.
func (p *Postgres) Claim(ctx context.Context) (Job, bool, error) {
	var job Job
	err := p.db.QueryRowContext(ctx, `
        WITH candidate AS (
            SELECT activity_id
            FROM activitypub_inbox_activities
            WHERE state = 'pending'
            ORDER BY created_at, activity_id
            FOR UPDATE SKIP LOCKED
            LIMIT 1
        )
        UPDATE activitypub_inbox_activities AS activity
        SET state = 'running', updated_at = now()
        FROM candidate
        WHERE activity.activity_id = candidate.activity_id
        RETURNING activity.activity_id, activity.route, activity.target, activity.actor_uri, activity.body`).Scan(
		&job.ActivityID, &job.Route, &job.Target, &job.ActorURI, &job.Body,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("claim pending activity: %w", err)
	}
	return job, true, nil
}

// Complete stores one terminal outcome. It cannot revive or overwrite an already-terminal record.
func (p *Postgres) Complete(ctx context.Context, activityID string, completion Completion) error {
	if err := validateCompletion(completion); err != nil {
		return err
	}
	result, err := p.db.ExecContext(ctx, `
        UPDATE activitypub_inbox_activities
        SET state = $2, location = $3, result = $4, body = '', updated_at = now()
        WHERE activity_id = $1 AND state IN ('pending', 'running')`,
		activityID, completion.State, completion.Location, append([]byte{}, completion.Result...))
	if err != nil {
		return fmt.Errorf("complete activity: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect activity completion: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("complete activity %q: expected one mutable record, updated %d", activityID, updated)
	}
	return nil
}

// LookupStatus resolves an opaque status capability without exposing searchable activity metadata.
func (p *Postgres) LookupStatus(ctx context.Context, token string) (Record, error) {
	return loadRecord(ctx, p.db, `status_token = $1`, token)
}

// LookupResult resolves the canonical public Activity IRI to its persisted exact bytes.
func (p *Postgres) LookupResult(ctx context.Context, location string) (Record, error) {
	return loadRecord(ctx, p.db, `location = $1 AND result <> ''`, location)
}

// Prune removes terminal payload hashes and cached outcomes after the configured retention even
// when no new inbox traffic arrives.
func (p *Postgres) Prune(ctx context.Context) error {
	if _, err := p.db.ExecContext(ctx, `
        DELETE FROM activitypub_inbox_activities
        WHERE state IN ('succeeded', 'denied', 'failed', 'ignored')
		  AND updated_at < now() - ($1 * interval '1 second')`, p.retention.Seconds()); err != nil {
		return fmt.Errorf("prune activity outcomes: %w", err)
	}
	return nil
}

// FailRunning terminalizes an interrupted attempt without replaying a possibly-spent A2A call.
func (p *Postgres) FailRunning(ctx context.Context) error {
	if _, err := p.db.ExecContext(ctx, `
        UPDATE activitypub_inbox_activities
		SET state = 'failed', body = '', updated_at = now()
        WHERE state = 'running'`); err != nil {
		return fmt.Errorf("terminalize interrupted activities: %w", err)
	}
	return nil
}

// Close closes the owned database pool.
func (p *Postgres) Close() error { return p.db.Close() }

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadRecord(ctx context.Context, querier rowQuerier, predicate string, value string) (Record, error) {
	var record Record
	query := `
		SELECT activity_id, route, target, actor_uri, body, body_hash, state, status_token, location, result, updated_at
        FROM activitypub_inbox_activities
        WHERE ` + predicate
	err := querier.QueryRowContext(ctx, query, value).Scan(
		&record.ActivityID, &record.Route, &record.Target, &record.ActorURI, &record.Body, &record.BodyHash,
		&record.State, &record.StatusToken, &record.Location, &record.Result, &record.Updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("load activity state: %w", err)
	}
	return record, nil
}
