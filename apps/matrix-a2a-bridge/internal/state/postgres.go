package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

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

// Conversation implements Store.
func (p *Postgres) Conversation(ctx context.Context, roomID, ghost string) (Conversation, bool, error) {
	var conversation Conversation
	var owners []byte
	err := p.db.QueryRow(ctx, `
		SELECT room_id, ghost, context_id, owners, owners_complete, updated_at
		FROM bridge_contexts
		WHERE room_id = $1 AND ghost = $2`, roomID, ghost).Scan(
		&conversation.RoomID, &conversation.Ghost, &conversation.ContextID, &owners,
		&conversation.OwnersComplete, &conversation.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, false, nil
	}
	if err != nil {
		return Conversation{}, false, fmt.Errorf("load conversation for %s/%s: %w", roomID, ghost, err)
	}
	if err := json.Unmarshal(owners, &conversation.Owners); err != nil {
		return Conversation{}, false, fmt.Errorf("decode conversation owners for %s/%s: %w", roomID, ghost, err)
	}
	return conversation, true, nil
}

// AddContextOwner implements Store.
func (p *Postgres) AddContextOwner(ctx context.Context, roomID, ghost, contextID, owner string) error {
	return p.db.DoTxn(ctx, nil, func(txCtx context.Context) error {
		conversation, found, err := p.lockConversation(txCtx, roomID, ghost)
		if err != nil {
			return err
		}
		if !found || conversation.ContextID != contextID {
			return ErrConversationChanged
		}
		if slices.Contains(conversation.Owners, owner) {
			return nil
		}
		if len(conversation.Owners) >= MaxConversationOwners {
			return ErrConversationOwnerLimit
		}
		conversation.Owners = append(conversation.Owners, owner)
		encoded, err := json.Marshal(conversation.Owners)
		if err != nil {
			return fmt.Errorf("encode conversation owners: %w", err)
		}
		if _, err := p.db.Exec(txCtx, `
			UPDATE bridge_contexts SET owners = $4, updated_at = now()
			WHERE room_id = $1 AND ghost = $2 AND context_id = $3`,
			roomID, ghost, contextID, encoded); err != nil {
			return fmt.Errorf("add context owner for %s/%s: %w", roomID, ghost, err)
		}
		return nil
	})
}

// SetContext implements Store.
func (p *Postgres) SetContext(ctx context.Context, roomID, ghost, contextID, owner string) error {
	return p.db.DoTxn(ctx, nil, func(txCtx context.Context) error {
		conversation, found, err := p.lockConversation(txCtx, roomID, ghost)
		if err != nil {
			return err
		}
		owners := []string{owner}
		ownersComplete := true
		if found && conversation.ContextID == contextID {
			owners = conversation.Owners
			ownersComplete = conversation.OwnersComplete
			if !slices.Contains(owners, owner) {
				if len(owners) >= MaxConversationOwners {
					return ErrConversationOwnerLimit
				}
				owners = append(owners, owner)
			}
		}
		encoded, err := json.Marshal(owners)
		if err != nil {
			return fmt.Errorf("encode conversation owners: %w", err)
		}
		if _, err := p.db.Exec(txCtx, `
			INSERT INTO bridge_contexts (room_id, ghost, context_id, owners, owners_complete, updated_at)
			VALUES ($1, $2, $3, $4, $5, now())
			ON CONFLICT (room_id, ghost) DO UPDATE
			SET context_id = EXCLUDED.context_id, owners = EXCLUDED.owners,
				owners_complete = EXCLUDED.owners_complete, updated_at = EXCLUDED.updated_at`,
			roomID, ghost, contextID, encoded, ownersComplete); err != nil {
			return fmt.Errorf("store context for %s/%s: %w", roomID, ghost, err)
		}
		return nil
	})
}

func (p *Postgres) lockConversation(ctx context.Context, roomID, ghost string) (Conversation, bool, error) {
	var conversation Conversation
	var owners []byte
	err := p.db.QueryRow(ctx, `
		SELECT room_id, ghost, context_id, owners, owners_complete, updated_at
		FROM bridge_contexts
		WHERE room_id = $1 AND ghost = $2
		FOR UPDATE`, roomID, ghost).Scan(
		&conversation.RoomID, &conversation.Ghost, &conversation.ContextID, &owners,
		&conversation.OwnersComplete, &conversation.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, false, nil
	}
	if err != nil {
		return Conversation{}, false, fmt.Errorf("lock conversation for %s/%s: %w", roomID, ghost, err)
	}
	if err := json.Unmarshal(owners, &conversation.Owners); err != nil {
		return Conversation{}, false, fmt.Errorf("decode conversation owners for %s/%s: %w", roomID, ghost, err)
	}
	return conversation, true, nil
}

// ConversationsBefore implements Store.
func (p *Postgres) ConversationsBefore(ctx context.Context, ghost string, cutoff time.Time, limit int) (_ []Conversation, returnedErr error) {
	rows, err := p.db.Query(ctx, `
		SELECT room_id, ghost, context_id, owners, owners_complete, updated_at
		FROM bridge_contexts
		WHERE ghost = $1 AND updated_at < $2
		ORDER BY updated_at, room_id
		LIMIT $3`, ghost, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired conversations for %s: %w", ghost, err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && returnedErr == nil {
			returnedErr = fmt.Errorf("close expired conversation rows: %w", closeErr)
		}
	}()
	conversations := make([]Conversation, 0, limit)
	for rows.Next() {
		var conversation Conversation
		var owners []byte
		if err := rows.Scan(
			&conversation.RoomID, &conversation.Ghost, &conversation.ContextID, &owners,
			&conversation.OwnersComplete, &conversation.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan expired conversation: %w", err)
		}
		if err := json.Unmarshal(owners, &conversation.Owners); err != nil {
			return nil, fmt.Errorf("decode expired conversation owners: %w", err)
		}
		conversations = append(conversations, conversation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired conversations: %w", err)
	}
	return conversations, nil
}

// DeleteConversation implements Store.
func (p *Postgres) DeleteConversation(ctx context.Context, conversation Conversation) (bool, error) {
	result, err := p.db.Exec(ctx, `
		DELETE FROM bridge_contexts
		WHERE room_id = $1 AND ghost = $2 AND context_id = $3 AND updated_at = $4`,
		conversation.RoomID, conversation.Ghost, conversation.ContextID, conversation.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("delete conversation for %s/%s: %w", conversation.RoomID, conversation.Ghost, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read deleted conversation count: %w", err)
	}
	return rows == 1, nil
}

// ConversationBusy implements Store.
func (p *Postgres) ConversationBusy(ctx context.Context, roomID, ghost string) (bool, error) {
	var busy bool
	if err := p.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM bridge_delegations
			WHERE room_id = $1 AND ghost_localpart = $2 AND terminal_at IS NULL
		)`, roomID, ghost).Scan(&busy); err != nil {
		return false, fmt.Errorf("check conversation work for %s/%s: %w", roomID, ghost, err)
	}
	return busy, nil
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

// MarkRoomWelcomed implements Store.
func (p *Postgres) MarkRoomWelcomed(ctx context.Context, roomID string) (bool, error) {
	res, err := p.db.Exec(ctx,
		"INSERT INTO bridge_room_welcomes (room_id) VALUES ($1) ON CONFLICT DO NOTHING",
		roomID)
	if err != nil {
		return false, fmt.Errorf("mark room %s welcomed: %w", roomID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark room %s welcomed: %w", roomID, err)
	}
	return inserted == 1, nil
}

// Close is a no-op: the shared dbutil pool is owned and closed by the caller (main).
func (p *Postgres) Close() error { return nil }
