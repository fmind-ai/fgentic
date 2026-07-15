package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.mau.fi/util/dbutil"
)

// Postgres implements Store on the shared dbutil database (the same connection pool that backs
// the mautrix SQL StateStore — one pool per pod, per SPEC §5).
type Postgres struct {
	db *dbutil.Database
}

// NewPostgres upgrades the versioned bridge schema and returns a Postgres store. The caller owns
// the root database handle's lifecycle; Close here is a no-op so the shared pool survives.
func NewPostgres(ctx context.Context, db *dbutil.Database) (*Postgres, error) {
	if db.Dialect != dbutil.Postgres {
		return nil, fmt.Errorf("bridge durable state requires Postgres, got %s", db.Dialect)
	}
	stateDB := db.Child(versionTableName, UpgradeTable, dbutil.NoopLogger)
	// database_owner is shared across child schemas, so retain the root database's owner rather
	// than inventing a child-specific owner that would conflict with other bridge tables.
	stateDB.Owner = db.Owner
	if err := stateDB.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade bridge state schema: %w", err)
	}
	return &Postgres{db: stateDB}, nil
}

// Context implements Store.
func (p *Postgres) Context(ctx context.Context, roomID, ghost string) (string, error) {
	var contextID string
	err := p.db.QueryRow(
		ctx,
		"SELECT context_id FROM bridge_contexts WHERE room_id = $1 AND ghost = $2",
		roomID, ghost,
	).Scan(&contextID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load context for %s/%s: %w", roomID, ghost, err)
	}
	return contextID, nil
}

// SetContext implements Store.
func (p *Postgres) SetContext(ctx context.Context, roomID, ghost, contextID string) error {
	_, err := p.db.Exec(ctx, `
		INSERT INTO bridge_contexts (room_id, ghost, context_id, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (room_id, ghost) DO UPDATE SET context_id = $3, updated_at = now()`,
		roomID, ghost, contextID)
	if err != nil {
		return fmt.Errorf("store context for %s/%s: %w", roomID, ghost, err)
	}
	return nil
}

// MarkEventProcessed implements Store.
func (p *Postgres) MarkEventProcessed(ctx context.Context, eventID string) (bool, error) {
	// Opportunistic prune: event volume is chat-scale, so the extra DELETE is negligible.
	if _, err := p.db.Exec(
		ctx,
		"DELETE FROM bridge_processed_events WHERE processed_at < now() - $1::interval",
		retention.String(),
	); err != nil {
		return false, fmt.Errorf("prune processed events: %w", err)
	}
	res, err := p.db.Exec(ctx,
		"INSERT INTO bridge_processed_events (event_id) VALUES ($1) ON CONFLICT DO NOTHING",
		eventID)
	if err != nil {
		return false, fmt.Errorf("mark event %s processed: %w", eventID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark event %s processed: %w", eventID, err)
	}
	return inserted == 1, nil
}

// Close is a no-op: the shared dbutil pool is owned and closed by the caller (main).
func (p *Postgres) Close() error { return nil }
