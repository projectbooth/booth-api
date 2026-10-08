// Package dbtest gives tests a real, isolated PostgreSQL to run against.
//
// The store tests run one shared contract suite against both the in-memory and the Postgres
// implementation, so they must hit a real database — a fake would defeat the point. Each
// test gets its own throwaway schema (its own tables, its own migrations bookkeeping), so
// tests are independent and can run in parallel against one server.
//
// The server is the one from hack/docker-compose.yml, found via
// BOOTH_TEST_POSTGRES_DSN (see hack/test-env.sh). With no DSN set, tests skip locally; in CI
// BOOTH_TEST_REQUIRE_POSTGRES=1 turns a missing database into a failure instead, so the
// Postgres path can never silently go untested on main.
package dbtest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var counter atomic.Int64

// Pool returns a pool whose search_path is a fresh, empty schema, dropped when the test ends.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_POSTGRES") == "1" {
			t.Fatal("BOOTH_TEST_POSTGRES_DSN is not set but BOOTH_TEST_REQUIRE_POSTGRES=1: the Postgres tests must run (see hack/test-env.sh)")
		}
		t.Skip("BOOTH_TEST_POSTGRES_DSN not set; start hack/docker-compose.yml and eval \"$(sh hack/test-env.sh)\" to run the Postgres tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to test postgres: %v", err)
	}
	t.Cleanup(admin.Close)

	// Schema names must be unique across the parallel test binaries `go test ./...` runs, so
	// mix the process ID into the counter.
	schema := fmt.Sprintf("t_%d_%d", os.Getpid(), counter.Add(1))
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing test DSN: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	// A test that leaks connections would otherwise starve the server across a long run.
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting to test schema: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pinging test schema: %v", strings.TrimSpace(err.Error()))
	}
	return pool
}
