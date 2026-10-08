// Package store is booth-api's persistence in its own database (ADR 0053): generated-API
// definitions and API keys. Every method takes the workspace explicitly and every query filters
// on it, so one workspace can never read or change another's rows, whatever id it supplies.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	ErrNotFound = errors.New("not found")
	// ErrExists: the dataset already has an API in this workspace.
	ErrExists = errors.New("this dataset already has an API")
	// ErrUnknownDatasets: a key was scoped to a dataset with no API in this workspace.
	ErrUnknownDatasets = errors.New("a key can only be scoped to datasets that have an API in this workspace")
)

// Store is the Postgres-backed store.
type Store struct{ pool *pgxpool.Pool }

// New returns the store. Call Migrate before serving anything from it.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Migrate applies the store's pending migrations. Safe to call from every replica at boot.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return db.Migrate(ctx, pool, "store", migrations, "migrations")
}

// ---- APIs --------------------------------------------------------------------------------

const apiColumns = `id, workspace, dataset_id, dataset_name, slug, table_schema, table_name, columns, primary_key, created_by, created_at, generated_at`

func scanAPI(row pgx.Row) (apidef.Definition, error) {
	var d apidef.Definition
	var cols, pk []byte
	err := row.Scan(&d.ID, &d.Workspace, &d.DatasetID, &d.DatasetName, &d.Slug, &d.Table.Schema, &d.Table.Name, &cols, &pk, &d.CreatedBy, &d.CreatedAt, &d.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(cols, &d.Columns); err != nil {
		return d, fmt.Errorf("decoding stored columns: %w", err)
	}
	if err := json.Unmarshal(pk, &d.PrimaryKey); err != nil {
		return d, fmt.Errorf("decoding stored primary key: %w", err)
	}
	return d, nil
}

// maxSlugAttempts bounds the "-2", "-3"... suffix search for a free slug.
const maxSlugAttempts = 50

// CreateAPI stores a new definition. Its slug is derived from the dataset name, with a numeric
// suffix if another API in the workspace already uses it. d's ID, Slug and timestamps are ignored
// and set from the stored row.
func (s *Store) CreateAPI(ctx context.Context, d apidef.Definition) (apidef.Definition, error) {
	cols, pk, err := marshalSnapshot(d)
	if err != nil {
		return d, err
	}
	for n := 1; n <= maxSlugAttempts; n++ {
		row := s.pool.QueryRow(ctx, `INSERT INTO apis (workspace, dataset_id, dataset_name, slug, table_schema, table_name, columns, primary_key, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+apiColumns,
			d.Workspace, d.DatasetID, d.DatasetName, apidef.Slug(d.DatasetName, n), d.Table.Schema, d.Table.Name, cols, pk, d.CreatedBy)
		out, err := scanAPI(row)
		switch constraint(err) {
		case "":
			return out, err
		case "apis_one_per_dataset":
			return d, ErrExists
		case "apis_slug_unique":
			continue
		default:
			return d, err
		}
	}
	return d, fmt.Errorf("no free slug for %q after %d attempts", d.DatasetName, maxSlugAttempts)
}

// UpdateSnapshot replaces an API's table snapshot (regeneration). The id, slug and keys are kept.
func (s *Store) UpdateSnapshot(ctx context.Context, d apidef.Definition) (apidef.Definition, error) {
	cols, pk, err := marshalSnapshot(d)
	if err != nil {
		return d, err
	}
	return scanAPI(s.pool.QueryRow(ctx, `UPDATE apis SET dataset_name = $3, table_schema = $4, table_name = $5, columns = $6, primary_key = $7, generated_at = now()
		WHERE workspace = $1 AND id = $2 RETURNING `+apiColumns,
		d.Workspace, d.ID, d.DatasetName, d.Table.Schema, d.Table.Name, cols, pk))
}

func marshalSnapshot(d apidef.Definition) (cols, pk []byte, err error) {
	if d.Columns == nil {
		d.Columns = []apidef.Column{}
	}
	if d.PrimaryKey == nil {
		d.PrimaryKey = []string{}
	}
	if cols, err = json.Marshal(d.Columns); err != nil {
		return nil, nil, err
	}
	pk, err = json.Marshal(d.PrimaryKey)
	return cols, pk, err
}

// GetAPI returns one API in the workspace.
func (s *Store) GetAPI(ctx context.Context, workspace, id string) (apidef.Definition, error) {
	return scanAPI(s.pool.QueryRow(ctx, `SELECT `+apiColumns+` FROM apis WHERE workspace = $1 AND id = $2`, workspace, id))
}

// GetAPIBySlug returns the API a public URL names (/v1/<slug>/...).
func (s *Store) GetAPIBySlug(ctx context.Context, workspace, slug string) (apidef.Definition, error) {
	return scanAPI(s.pool.QueryRow(ctx, `SELECT `+apiColumns+` FROM apis WHERE workspace = $1 AND slug = $2`, workspace, slug))
}

// ListAPIs returns the workspace's APIs, by dataset name.
func (s *Store) ListAPIs(ctx context.Context, workspace string) ([]apidef.Definition, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+apiColumns+` FROM apis WHERE workspace = $1 ORDER BY lower(dataset_name), slug`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []apidef.Definition{}
	for rows.Next() {
		d, err := scanAPI(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteAPI removes an API and drops it from every key's scope in the same transaction, so a
// later API for the same dataset starts with no keys (see the migration).
func (s *Store) DeleteAPI(ctx context.Context, workspace, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var dataset string
		err := tx.QueryRow(ctx, `DELETE FROM apis WHERE workspace = $1 AND id = $2 RETURNING dataset_id`, workspace, id).Scan(&dataset)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM api_key_datasets kd USING api_keys k
			WHERE kd.key_id = k.id AND k.workspace = $1 AND kd.dataset_id = $2`, workspace, dataset)
		return err
	})
}

// ---- keys --------------------------------------------------------------------------------

// Key is a stored API key. SecretHash is only ever read back for verification.
type Key struct {
	ID            string     `json:"id"`
	Workspace     string     `json:"-"`
	Name          string     `json:"name"`
	SecretHash    []byte     `json:"-"`
	CreatedBy     string     `json:"createdBy"`
	CreatedByName string     `json:"createdByName"`
	CreatedAt     time.Time  `json:"createdAt"`
	RevokedAt     *time.Time `json:"revokedAt"`
	RevokedBy     string     `json:"revokedBy,omitempty"`
	LastUsedAt    *time.Time `json:"lastUsedAt"`
	DatasetIDs    []string   `json:"datasetIds"`
}

// CreateKey stores a key and its dataset scope atomically. Every dataset must have an API in the
// key's workspace; that check runs inside the transaction, so a concurrent API delete can't leave
// a key scoped to a dataset it was never allowed to name.
func (s *Store) CreateKey(ctx context.Context, k Key) (Key, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// FOR SHARE holds the API rows until commit, so DeleteAPI waits for this key to land and
		// then removes its scope row along with everyone else's. DatasetIDs is deduplicated by
		// the caller (internal/keys), so a row count is an exact check.
		rows, err := tx.Query(ctx, `SELECT dataset_id FROM apis WHERE workspace = $1 AND dataset_id = ANY($2) FOR SHARE`, k.Workspace, k.DatasetIDs)
		if err != nil {
			return err
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(found) != len(k.DatasetIDs) {
			return ErrUnknownDatasets
		}
		if err := tx.QueryRow(ctx, `INSERT INTO api_keys (id, workspace, name, secret_hash, created_by, created_by_name)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
			k.ID, k.Workspace, k.Name, k.SecretHash, k.CreatedBy, k.CreatedByName).Scan(&k.CreatedAt); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO api_key_datasets (key_id, dataset_id) SELECT $1, unnest($2::text[])`, k.ID, k.DatasetIDs)
		return err
	})
	return k, err
}

const keyQuery = `SELECT k.id, k.workspace, k.name, k.secret_hash, k.created_by, k.created_by_name, k.created_at, k.revoked_at, coalesce(k.revoked_by, ''), k.last_used_at,
	coalesce(array_agg(kd.dataset_id ORDER BY kd.dataset_id COLLATE "C") FILTER (WHERE kd.dataset_id IS NOT NULL), '{}')
	FROM api_keys k LEFT JOIN api_key_datasets kd ON kd.key_id = k.id`

func scanKey(row pgx.Row) (Key, error) {
	var k Key
	err := row.Scan(&k.ID, &k.Workspace, &k.Name, &k.SecretHash, &k.CreatedBy, &k.CreatedByName, &k.CreatedAt, &k.RevokedAt, &k.RevokedBy, &k.LastUsedAt, &k.DatasetIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return k, ErrNotFound
	}
	return k, err
}

// ListKeys returns the workspace's keys, revoked ones included (newest first). Hashes are loaded
// but never serialized (json:"-").
func (s *Store) ListKeys(ctx context.Context, workspace string) ([]Key, error) {
	rows, err := s.pool.Query(ctx, keyQuery+` WHERE k.workspace = $1 GROUP BY k.id ORDER BY k.created_at DESC, k.id`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// KeyByID looks a key up by its public id alone. Used only for verification, where the workspace
// is not known until the key is found (ADR 0101: it comes from the key, never a header).
func (s *Store) KeyByID(ctx context.Context, id string) (Key, error) {
	return scanKey(s.pool.QueryRow(ctx, keyQuery+` WHERE k.id = $1 GROUP BY k.id`, id))
}

// RevokeKey revokes a key in the workspace. Revoking twice keeps the first revocation's time and
// actor and is not an error.
func (s *Store) RevokeKey(ctx context.Context, workspace, id, by string) (Key, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now(), revoked_by = $3
		WHERE workspace = $1 AND id = $2 AND revoked_at IS NULL`, workspace, id, by)
	if err != nil {
		return Key{}, err
	}
	k, err := scanKey(s.pool.QueryRow(ctx, keyQuery+` WHERE k.workspace = $1 AND k.id = $2 GROUP BY k.id`, workspace, id))
	if err == nil && tag.RowsAffected() == 0 && k.RevokedAt == nil {
		return k, fmt.Errorf("revoking key %s: no row updated", id)
	}
	return k, err
}

// TouchKey records that a key was used, at most once per minute per key, so a busy key doesn't
// turn every read into a write.
func (s *Store) TouchKey(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, id)
	return err
}

// constraint returns the violated unique constraint's name, or "" for no error / another error.
func constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	if err != nil {
		return "?"
	}
	return ""
}
