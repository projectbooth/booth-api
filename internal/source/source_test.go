package source

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/db/dbtest"
)

// The fixture stands in for a table in a workspace's booth-database database until the catalog
// can name one (ADR 0102) and the sidecar path exists (ADR 0103).
func TestIntrospect(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE orders (
			region   text NOT NULL,
			id       bigint NOT NULL,
			dropped  int,
			"Total Amount" numeric(12,2),
			placed   timestamptz NOT NULL DEFAULT now(),
			tags     text[],
			PRIMARY KEY (region, id)
		);
		ALTER TABLE orders DROP COLUMN dropped;
		CREATE TABLE no_pk (x int);
		CREATE VIEW order_view AS SELECT id FROM orders;`); err != nil {
		t.Fatal(err)
	}

	got, err := Introspect(ctx, pool, apidef.TableRef{Schema: schema, Name: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	want := []apidef.Column{
		{Name: "region", Type: "text"},
		{Name: "id", Type: "bigint"},
		{Name: "Total Amount", Type: "numeric(12,2)", Nullable: true},
		{Name: "placed", Type: "timestamp with time zone"},
		{Name: "tags", Type: "text[]", Nullable: true},
	}
	if !reflect.DeepEqual(got.Columns, want) {
		t.Errorf("columns = %+v\nwant      %+v", got.Columns, want)
	}
	// Key order, not column order (id is declared after region in the key but both orders agree
	// here, so also check a key declared in reverse).
	if !reflect.DeepEqual(got.PrimaryKey, []string{"region", "id"}) {
		t.Errorf("primary key = %v", got.PrimaryKey)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE rev (a int, b int, PRIMARY KEY (b, a))`); err != nil {
		t.Fatal(err)
	}
	if rev, err := Introspect(ctx, pool, apidef.TableRef{Schema: schema, Name: "rev"}); err != nil || !reflect.DeepEqual(rev.PrimaryKey, []string{"b", "a"}) {
		t.Errorf("rev primary key = %v, %v; want key order [b a]", rev.PrimaryKey, err)
	}

	if np, err := Introspect(ctx, pool, apidef.TableRef{Schema: schema, Name: "no_pk"}); err != nil || len(np.PrimaryKey) != 0 {
		t.Errorf("no_pk = %+v, %v", np, err)
	}
	if v, err := Introspect(ctx, pool, apidef.TableRef{Schema: schema, Name: "order_view"}); err != nil || len(v.Columns) != 1 {
		t.Errorf("view = %+v, %v", v, err)
	}
	for _, ref := range []apidef.TableRef{
		{Schema: schema, Name: "missing"},
		{Schema: "no_such_schema", Name: "orders"},
		{Schema: schema, Name: "orders; DROP TABLE orders"},
	} {
		if _, err := Introspect(ctx, pool, ref); !errors.Is(err, ErrTableNotFound) {
			t.Errorf("%+v: err = %v, want ErrTableNotFound", ref, err)
		}
	}
}

func TestIntrospect_UnselectableTableIsNotFound(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	// A role that can see the schema but not select from the table, like a read lease facing a
	// table outside what it was granted.
	role := schema + "_ro"
	for _, q := range []string{
		`CREATE TABLE secret (x int)`,
		`CREATE ROLE ` + role,
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP OWNED BY `+role+`; DROP ROLE `+role) })
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE `+role); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `RESET ROLE`) //nolint:errcheck
	if _, err := Introspect(ctx, conn, apidef.TableRef{Schema: schema, Name: "secret"}); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("a table the role can't SELECT: err = %v, want ErrTableNotFound", err)
	}
}
