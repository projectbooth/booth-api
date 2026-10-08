package gql

import (
	"strings"
	"testing"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/table"
)

// Every shape of table must yield SDL gqlparser accepts: with and without a key, with nothing
// orderable, with only list/JSON columns.
func TestSDLLoads(t *testing.T) {
	for name, def := range map[string]apidef.Definition{
		"typical": {Slug: "orders", DatasetName: `Orders "quoted" """`, Columns: []apidef.Column{
			{Name: "id", Type: "bigint", Description: `the """id"""`}, {Name: "amount", Type: "numeric(12,2)", Nullable: true},
			{Name: "placed", Type: "timestamp with time zone"}, {Name: "tags", Type: "text[]", Nullable: true}, {Name: "meta", Type: "jsonb", Nullable: true},
			{Name: "u", Type: "uuid"}, {Name: "d", Type: "date", Nullable: true}, {Name: "t", Type: "time without time zone", Nullable: true},
			{Name: "ok", Type: "boolean"}, {Name: "f", Type: "double precision", Nullable: true},
		}, PrimaryKey: []string{"id"}},
		"composite key":     {Slug: "c", Columns: []apidef.Column{{Name: "a", Type: "text"}, {Name: "b", Type: "integer"}}, PrimaryKey: []string{"a", "b"}},
		"nothing orderable": {Slug: "n", Columns: []apidef.Column{{Name: "x", Type: "text", Nullable: true}, {Name: "j", Type: "jsonb"}}},
		"only json":         {Slug: "j", Columns: []apidef.Column{{Name: "j", Type: "jsonb"}}},
	} {
		e, err := New(table.NewModel(def), DefaultLimits)
		if err != nil {
			t.Errorf("%s: %v\n%s", name, err, SDL(table.NewModel(def)))
			continue
		}
		hasRow := strings.Contains(e.SDL, "row(")
		if want := len(def.PrimaryKey) == 1; hasRow != want {
			t.Errorf("%s: row(pk) present = %v, want %v", name, hasRow, want)
		}
	}
	if _, err := New(table.NewModel(apidef.Definition{Slug: "z", Columns: []apidef.Column{{Name: "b", Type: "bytea"}}}), DefaultLimits); err == nil {
		t.Error("a table with no exposable column produced an engine")
	}
}
