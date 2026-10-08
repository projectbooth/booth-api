// Package source reads a workspace's booth-database tables: today, introspection of one table's
// columns and primary key for a generated API's snapshot (docs/design-v0.md §3).
//
// Where the connection comes from is behind Pools. In production that will be a per-workspace pool
// on the credential sidecar's loopback socket (ADR 0103), which is not built: core has not yet
// confirmed the workspace-membership bound on workload-token minting (ADR 0103 item 4), so the
// module does not request `mint: true`. Until then production runs with Unavailable, and the code
// here is exercised against a fixture table in tests.
package source

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-api/internal/apidef"
)

var (
	// ErrUnavailable: no data source is configured for workspaces yet.
	ErrUnavailable = errors.New("reading workspace databases is not available yet")
	// ErrTableNotFound: no such table, or not one the workspace's read lease can select from.
	ErrTableNotFound = errors.New("table not found in the workspace database")
)

// Pools hands out a connection pool on a workspace's database.
type Pools interface {
	Pool(ctx context.Context, workspace string) (*pgxpool.Pool, error)
}

// Unavailable is the production Pools until the sidecar wiring lands.
type Unavailable struct{}

func (Unavailable) Pool(context.Context, string) (*pgxpool.Pool, error) { return nil, ErrUnavailable }

// Querier is what Introspect needs: a *pgxpool.Pool or one acquired connection.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Table is what introspection found.
type Table struct {
	Columns    []apidef.Column
	PrimaryKey []string
}

// Introspect reads a table's columns (in column order) and primary key. A table the connection's
// role cannot SELECT from is reported as not found, the same as a missing one: a read lease covers
// every table in the workspace's public schema (ADR 0103), so anything else is not this API's to
// expose. Schema and table are passed as parameters, never interpolated.
func Introspect(ctx context.Context, pool Querier, ref apidef.TableRef) (Table, error) {
	var oid uint32
	err := pool.QueryRow(ctx, `SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		AND has_table_privilege(c.oid, 'SELECT')`, ref.Schema, ref.Name).Scan(&oid)
	if errors.Is(err, pgx.ErrNoRows) {
		return Table{}, ErrTableNotFound
	}
	if err != nil {
		return Table{}, err
	}

	rows, err := pool.Query(ctx, `SELECT attname, format_type(atttypid, atttypmod), NOT attnotnull
		FROM pg_attribute WHERE attrelid = $1 AND attnum > 0 AND NOT attisdropped ORDER BY attnum`, oid)
	if err != nil {
		return Table{}, err
	}
	cols, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (apidef.Column, error) {
		var c apidef.Column
		err := r.Scan(&c.Name, &c.Type, &c.Nullable)
		return c, err
	})
	if err != nil {
		return Table{}, err
	}

	rows, err = pool.Query(ctx, `SELECT a.attname FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
		WHERE i.indrelid = $1 AND i.indisprimary
		ORDER BY array_position(i.indkey::int2[], a.attnum)`, oid)
	if err != nil {
		return Table{}, err
	}
	pk, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return Table{}, err
	}
	return Table{Columns: cols, PrimaryKey: pk}, nil
}
