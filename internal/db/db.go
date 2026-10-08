// Package db opens booth-api's own database (the one booth-core provisions for it, ADR 0053) and
// applies embedded migrations. Copied from booth-catalog's internal/db, minus its search helper:
// one migrations table keyed by component, applied in one transaction under an advisory lock, so
// every replica can migrate at boot.
package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockID is an arbitrary constant identifying booth-api's migration lock in
// Postgres's advisory-lock keyspace, so replicas starting at the same time serialize
// instead of racing to create the same table.
const migrationLockID int64 = 0x626f6f7461706931 // "bootapi1"

// Open connects to dsn and verifies the connection.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return pool, nil
}

// Migrate applies the component's pending migrations from dir within fsys, in filename
// order, each exactly once. Filenames must start with a numeric version ("0001_x.sql").
// It runs in one transaction holding an advisory lock, so it is safe to call from every
// replica at boot.
func Migrate(ctx context.Context, pool *pgxpool.Pool, component string, fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("reading %s migrations: %w", component, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting %s migration transaction: %w", component, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("taking migration lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS api_schema_migrations (
		component  TEXT        NOT NULL,
		version    INT         NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (component, version)
	)`); err != nil {
		return fmt.Errorf("creating migrations table: %w", err)
	}

	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("%s migration %q: filename must start with a numeric version", component, name)
		}
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_schema_migrations WHERE component = $1 AND version = $2)`, component, version).Scan(&applied); err != nil {
			return fmt.Errorf("checking %s migration %s: %w", component, name, err)
		}
		if applied {
			continue
		}
		sql, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("applying %s migration %s: %w", component, name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO api_schema_migrations (component, version) VALUES ($1, $2)`, component, version); err != nil {
			return fmt.Errorf("recording %s migration %s: %w", component, name, err)
		}
	}
	return tx.Commit(ctx)
}
