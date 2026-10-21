package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/projectbooth/booth-api/internal/apidef"
)

// ErrWritesOff: the API has no write settings, so nothing can be granted on it.
var ErrWritesOff = errors.New("writes are off for this API; an owner must turn them on first")

// ---- write settings (ADR 0117) -----------------------------------------------------------

// SetWriteSettings creates or replaces an API's write settings, bumping the version.
func (s *Store) SetWriteSettings(ctx context.Context, workspace, apiID string, ops, cols []string, by string) (apidef.WriteSettings, error) {
	var w apidef.WriteSettings
	err := s.pool.QueryRow(ctx, `INSERT INTO api_writes (api_id, operations, columns, updated_by)
		SELECT id, $3, $4, $5 FROM apis WHERE workspace = $1 AND id = $2
		ON CONFLICT (api_id) DO UPDATE SET operations = EXCLUDED.operations, columns = EXCLUDED.columns,
			updated_by = EXCLUDED.updated_by, updated_at = now(), version = api_writes.version + 1
		RETURNING operations, columns, version, updated_by, updated_at`,
		workspace, apiID, ops, cols, by).Scan(&w.Operations, &w.Columns, &w.Version, &w.UpdatedBy, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, ErrNotFound
	}
	return w, err
}

// DeleteWriteSettings turns writes off for an API and revokes every live grant on it, in one
// transaction. It returns the grants it revoked; existed is false if writes were already off.
func (s *Store) DeleteWriteSettings(ctx context.Context, workspace, apiID, by string) (revoked []Grant, existed bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM apis WHERE workspace = $1 AND id = $2)`, workspace, apiID).Scan(&found); err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		tag, err := tx.Exec(ctx, `DELETE FROM api_writes WHERE api_id = $1`, apiID)
		if err != nil {
			return err
		}
		existed = tag.RowsAffected() > 0
		rows, err := tx.Query(ctx, `UPDATE write_grants SET revoked_at = now(), revoked_by = $3
			WHERE workspace = $1 AND api_id = $2 AND revoked_at IS NULL RETURNING `+grantColumns, workspace, apiID, by)
		if err != nil {
			return err
		}
		revoked, err = collectGrants(rows)
		return err
	})
	return revoked, existed, err
}

// ---- grants ------------------------------------------------------------------------------

// Grant is a write grant: principal may make the listed operations on the API, as granted_by.
type Grant struct {
	ID         string   `json:"id"`
	Workspace  string   `json:"-"`
	APIID      string   `json:"apiId"`
	Principal  string   `json:"principal"`
	Operations []string `json:"operations"`
	// Columns narrows the API's writable columns; nil means all of them.
	Columns       []string   `json:"columns"`
	GrantedBy     string     `json:"grantedBy"`
	GrantedByName string     `json:"grantedByName"`
	CreatedAt     time.Time  `json:"createdAt"`
	RevokedAt     *time.Time `json:"revokedAt"`
	RevokedBy     string     `json:"revokedBy,omitempty"`
}

const grantColumns = `id, workspace, api_id, principal, operations, columns, granted_by, granted_by_name, created_at, revoked_at, coalesce(revoked_by, '')`

func scanGrant(row pgx.Row) (Grant, error) {
	var g Grant
	err := row.Scan(&g.ID, &g.Workspace, &g.APIID, &g.Principal, &g.Operations, &g.Columns, &g.GrantedBy, &g.GrantedByName, &g.CreatedAt, &g.RevokedAt, &g.RevokedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, ErrNotFound
	}
	return g, err
}

func collectGrants(rows pgx.Rows) ([]Grant, error) {
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateGrant stores a grant. A live grant for the same API and principal is revoked in the same
// transaction (replaced is it, or nil). Writes must be on for the API (ErrWritesOff).
func (s *Store) CreateGrant(ctx context.Context, g Grant) (created Grant, replaced *Grant, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// FOR SHARE holds the settings row, so turning writes off waits for this grant and then
		// revokes it with the rest.
		var ok bool
		err := tx.QueryRow(ctx, `SELECT true FROM api_writes w JOIN apis a ON a.id = w.api_id
			WHERE a.workspace = $1 AND a.id = $2 FOR SHARE OF w`, g.Workspace, g.APIID).Scan(&ok)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM apis WHERE workspace = $1 AND id = $2)`, g.Workspace, g.APIID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrNotFound
			}
			return ErrWritesOff
		}
		if err != nil {
			return err
		}
		old, err := scanGrant(tx.QueryRow(ctx, `UPDATE write_grants SET revoked_at = now(), revoked_by = $4
			WHERE workspace = $1 AND api_id = $2 AND principal = $3 AND revoked_at IS NULL RETURNING `+grantColumns,
			g.Workspace, g.APIID, g.Principal, g.GrantedBy))
		switch {
		case err == nil:
			replaced = &old
		case !errors.Is(err, ErrNotFound):
			return err
		}
		created, err = scanGrant(tx.QueryRow(ctx, `INSERT INTO write_grants (workspace, api_id, principal, operations, columns, granted_by, granted_by_name)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+grantColumns,
			g.Workspace, g.APIID, g.Principal, g.Operations, g.Columns, g.GrantedBy, g.GrantedByName))
		return err
	})
	return created, replaced, err
}

// GrantFilter narrows ListGrants. Empty fields match everything.
type GrantFilter struct {
	APIID          string
	Principal      string
	IncludeRevoked bool
}

// ListGrants returns the workspace's grants, newest first.
func (s *Store) ListGrants(ctx context.Context, workspace string, f GrantFilter) ([]Grant, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+grantColumns+` FROM write_grants
		WHERE workspace = $1 AND ($2 = '' OR api_id = $2) AND ($3 = '' OR principal = $3) AND ($4 OR revoked_at IS NULL)
		ORDER BY created_at DESC, id`, workspace, f.APIID, f.Principal, f.IncludeRevoked)
	if err != nil {
		return nil, err
	}
	return collectGrants(rows)
}

// LiveGrant returns principal's live grant on the API, or ErrNotFound.
func (s *Store) LiveGrant(ctx context.Context, workspace, apiID, principal string) (Grant, error) {
	return scanGrant(s.pool.QueryRow(ctx, `SELECT `+grantColumns+` FROM write_grants
		WHERE workspace = $1 AND api_id = $2 AND principal = $3 AND revoked_at IS NULL`, workspace, apiID, principal))
}

// RevokeGrant revokes one grant. Revoking twice keeps the first revocation and is not an error.
func (s *Store) RevokeGrant(ctx context.Context, workspace, id, by string) (Grant, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE write_grants SET revoked_at = now(), revoked_by = $3
		WHERE workspace = $1 AND id = $2 AND revoked_at IS NULL`, workspace, id, by); err != nil {
		return Grant{}, err
	}
	return scanGrant(s.pool.QueryRow(ctx, `SELECT `+grantColumns+` FROM write_grants WHERE workspace = $1 AND id = $2`, workspace, id))
}

// RevokePrincipal revokes every live grant principal holds in the workspace, and returns them.
func (s *Store) RevokePrincipal(ctx context.Context, workspace, principal, by string) ([]Grant, error) {
	rows, err := s.pool.Query(ctx, `UPDATE write_grants SET revoked_at = now(), revoked_by = $3
		WHERE workspace = $1 AND principal = $2 AND revoked_at IS NULL RETURNING `+grantColumns, workspace, principal, by)
	if err != nil {
		return nil, err
	}
	return collectGrants(rows)
}

// HasLiveGrantsBy reports whether owner still has a live grant on the API: their write sidecar is
// stopped when the last one goes.
func (s *Store) HasLiveGrantsBy(ctx context.Context, apiID, owner string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM write_grants WHERE api_id = $1 AND granted_by = $2 AND revoked_at IS NULL)`, apiID, owner).Scan(&ok)
	return ok, err
}

// ---- idempotency -------------------------------------------------------------------------

// Idempotent is a stored idempotency record.
type Idempotent struct {
	RequestHash string
	State       string // "pending" or "done"
	Response    json.RawMessage
	CreatedAt   time.Time
}

// ClaimIdempotencyKey records a pending write under (API, principal, key). If the key is already
// in use (and younger than ttl), it returns that record and claimed=false; an older one is
// replaced, so a key is reusable after ttl.
func (s *Store) ClaimIdempotencyKey(ctx context.Context, apiID, principal, key, hash string, ttl time.Duration) (rec Idempotent, claimed bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM write_idempotency WHERE api_id = $1 AND principal = $2 AND key = $3
			AND created_at < now() - make_interval(secs => $4)`, apiID, principal, key, ttl.Seconds()); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO write_idempotency (api_id, principal, key, request_hash, state)
			VALUES ($1, $2, $3, $4, 'pending') ON CONFLICT DO NOTHING`, apiID, principal, key, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			claimed = true
			return nil
		}
		return tx.QueryRow(ctx, `SELECT request_hash, state, response, created_at FROM write_idempotency
			WHERE api_id = $1 AND principal = $2 AND key = $3`, apiID, principal, key).Scan(&rec.RequestHash, &rec.State, &rec.Response, &rec.CreatedAt)
	})
	return rec, claimed, err
}

// ReleaseIdempotencyKey forgets a claimed key whose write certainly did not happen (refused
// before or rolled back), so the client can retry with the same key.
func (s *Store) ReleaseIdempotencyKey(ctx context.Context, apiID, principal, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM write_idempotency WHERE api_id = $1 AND principal = $2 AND key = $3 AND state = 'pending'`, apiID, principal, key)
	return err
}

// ---- audit -------------------------------------------------------------------------------

// WriteLogEntry is one audited write. It has no field for row values.
type WriteLogEntry struct {
	Workspace, APIID, Operation, Principal, GrantedBy string
	ReportedUser, RowKeyHash, IdempotencyKey          string
	Status                                            int
	Outcome                                           string
	Duration                                          time.Duration
}

// LogWrite appends to the write audit log.
func (s *Store) LogWrite(ctx context.Context, e WriteLogEntry) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO write_log (workspace, api_id, operation, principal, granted_by, reported_user, row_key_hash, idempotency_key, status, outcome, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		e.Workspace, e.APIID, e.Operation, e.Principal, e.GrantedBy, e.ReportedUser, e.RowKeyHash, e.IdempotencyKey, e.Status, e.Outcome, int(e.Duration.Milliseconds()))
	return err
}

// IdempotencyResult is a claimed key's result, stored when its write commits.
type IdempotencyResult struct {
	APIID, Principal, Key string
	Response              json.RawMessage
}

// RecordWrite records a committed write in one transaction: its audit entry, its idempotency key's
// result (idem, if the write had a key) and its event for the outbox (ev, if events are on). So an
// event is never queued for a write the audit log or the key doesn't show as done.
func (s *Store) RecordWrite(ctx context.Context, e WriteLogEntry, idem *IdempotencyResult, ev *OutboxEvent) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO write_log (workspace, api_id, operation, principal, granted_by, reported_user, row_key_hash, idempotency_key, status, outcome, duration_ms)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			e.Workspace, e.APIID, e.Operation, e.Principal, e.GrantedBy, e.ReportedUser, e.RowKeyHash, e.IdempotencyKey, e.Status, e.Outcome, int(e.Duration.Milliseconds())); err != nil {
			return err
		}
		if idem != nil {
			if _, err := tx.Exec(ctx, `UPDATE write_idempotency SET state = 'done', response = $4
				WHERE api_id = $1 AND principal = $2 AND key = $3`, idem.APIID, idem.Principal, idem.Key, idem.Response); err != nil {
				return err
			}
		}
		if ev != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO event_outbox (workspace, event_type, data) VALUES ($1, $2, $3)`, ev.Workspace, ev.EventType, ev.Data); err != nil {
				return err
			}
		}
		return nil
	})
}

// LogAdmin records a change to write settings or grants.
func (s *Store) LogAdmin(ctx context.Context, workspace, apiID, actor, action string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO write_admin_log (workspace, api_id, actor, action, detail) VALUES ($1, $2, $3, $4, $5)`,
		workspace, apiID, actor, action, b)
	return err
}

// WriteLog returns an API's most recent audited writes (newest first), at most limit.
func (s *Store) WriteLog(ctx context.Context, workspace, apiID string, limit int) ([]WriteLogRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT at, operation, principal, granted_by, reported_user, row_key_hash, idempotency_key, status, outcome, duration_ms
		FROM write_log WHERE workspace = $1 AND api_id = $2 ORDER BY at DESC, id DESC LIMIT $3`, workspace, apiID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WriteLogRecord{}
	for rows.Next() {
		var r WriteLogRecord
		if err := rows.Scan(&r.At, &r.Operation, &r.Principal, &r.GrantedBy, &r.ReportedUser, &r.RowKeyHash, &r.IdempotencyKey, &r.Status, &r.Outcome, &r.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WriteLogRecord is one row of the write audit log as the management API shows it.
type WriteLogRecord struct {
	At             time.Time `json:"at"`
	Operation      string    `json:"operation"`
	Principal      string    `json:"principal"`
	GrantedBy      string    `json:"grantedBy"`
	ReportedUser   string    `json:"reportedUser,omitempty"`
	RowKeyHash     string    `json:"rowKeyHash,omitempty"`
	IdempotencyKey string    `json:"idempotencyKey,omitempty"`
	Status         int       `json:"status"`
	Outcome        string    `json:"outcome"`
	DurationMS     int       `json:"durationMs"`
}

// Prune deletes audit entries older than retention and idempotency records older than idemTTL.
func (s *Store) Prune(ctx context.Context, retention, idemTTL time.Duration) (int64, error) {
	var n int64
	for _, q := range []struct {
		sql string
		age time.Duration
	}{
		{`DELETE FROM write_log WHERE at < now() - make_interval(secs => $1)`, retention},
		{`DELETE FROM write_admin_log WHERE at < now() - make_interval(secs => $1)`, retention},
		{`DELETE FROM write_idempotency WHERE created_at < now() - make_interval(secs => $1)`, idemTTL},
	} {
		tag, err := s.pool.Exec(ctx, q.sql, q.age.Seconds())
		if err != nil {
			return n, err
		}
		n += tag.RowsAffected()
	}
	return n, nil
}
