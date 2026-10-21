package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- event outbox (ADR 0123) -------------------------------------------------------------

// OutboxEvent is an event queued for the event bus: its envelope's workspace and type, and data.
type OutboxEvent struct {
	Workspace, EventType string
	Data                 json.RawMessage
}

// OutboxRow is a queued event as the relay reads it.
type OutboxRow struct {
	ID        int64
	MsgID     string
	Workspace string
	EventType string
	Data      json.RawMessage
	CreatedAt time.Time
}

// RelayOutbox hands up to limit queued events, oldest first, to publish, and deletes each one
// publish accepted. It stops at the first refusal and returns how many were sent and that error.
// Rows are claimed with FOR UPDATE SKIP LOCKED, so replicas relaying at once never send the same
// event together.
func (s *Store) RelayOutbox(ctx context.Context, limit int, publish func(OutboxRow) error) (int, error) {
	var sent []int64
	var perr error
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, msg_id, workspace, event_type, data, created_at FROM event_outbox
			ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		batch, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OutboxRow, error) {
			var o OutboxRow
			err := r.Scan(&o.ID, &o.MsgID, &o.Workspace, &o.EventType, &o.Data, &o.CreatedAt)
			return o, err
		})
		if err != nil {
			return err
		}
		for _, o := range batch {
			if perr = publish(o); perr != nil {
				break
			}
			sent = append(sent, o.ID)
		}
		if len(sent) == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `DELETE FROM event_outbox WHERE id = ANY($1)`, sent)
		return err
	})
	if err != nil {
		return 0, err
	}
	return len(sent), perr
}

// ExpireOutbox deletes queued events older than maxAge, undelivered, and returns how many.
func (s *Store) ExpireOutbox(ctx context.Context, maxAge time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM event_outbox WHERE created_at < now() - make_interval(secs => $1)`, maxAge.Seconds())
	return tag.RowsAffected(), err
}
