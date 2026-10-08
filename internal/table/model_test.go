package table

import (
	"strings"
	"testing"

	"github.com/projectbooth/booth-api/internal/apidef"
)

func TestScalarFor(t *testing.T) {
	for pg, want := range map[string]struct {
		s    Scalar
		list bool
		ok   bool
	}{
		"smallint": {Int, false, true}, "integer": {Int, false, true}, "bigint": {BigInt, false, true},
		"numeric": {Decimal, false, true}, "numeric(12,2)": {Decimal, false, true},
		"real": {Float, false, true}, "double precision": {Float, false, true}, "boolean": {Boolean, false, true},
		"text": {String, false, true}, "character varying(40)": {String, false, true}, "character(3)": {String, false, true},
		"uuid": {ID, false, true}, "date": {Date, false, true},
		"timestamp without time zone": {DateTime, false, true}, "timestamp(3) with time zone": {DateTime, false, true},
		"time without time zone": {Time, false, true}, "jsonb": {JSON, false, true}, "json": {JSON, false, true},
		"text[]": {String, true, true}, "bigint[]": {BigInt, true, true},
		"integer[][]": {"", false, false}, "bytea": {"", false, false}, "money": {"", false, false},
		"interval": {"", false, false}, "time with time zone": {"", false, false}, "inet": {"", false, false}, "mood": {"", false, false},
	} {
		s, list, ok := scalarFor(pg)
		if s != want.s || list != want.list || ok != want.ok {
			t.Errorf("scalarFor(%q) = %q,%v,%v want %q,%v,%v", pg, s, list, ok, want.s, want.list, want.ok)
		}
	}
}

func TestNamesAndOmissions(t *testing.T) {
	m := NewModel(apidef.Definition{
		Columns: []apidef.Column{
			{Name: "id", Type: "bigint"},
			{Name: "Total Amount", Type: "integer", Nullable: true},
			{Name: "Total_Amount", Type: "integer", Nullable: true},
			{Name: "1st", Type: "text"},
			{Name: "__secret", Type: "text"},
			{Name: "and", Type: "text"},
			{Name: "naïve", Type: "text"},
			{Name: "blob", Type: "bytea"},
			{Name: "grid", Type: "integer[][]"},
		},
		PrimaryKey: []string{"id"},
	})
	var names []string
	for _, f := range m.Fields {
		names = append(names, f.Name+"="+f.Column)
	}
	want := "id=id Total_Amount=Total Amount Total_Amount_2=Total_Amount _1st=1st _secret=__secret and_=and na_ve=naïve"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("names:\n got  %s\n want %s", got, want)
	}
	if len(m.Omitted) != 2 || m.Omitted[0].Column != "blob" || m.Omitted[1].Column != "grid" {
		t.Errorf("omitted = %v", m.Omitted)
	}
	if len(m.PK) != 1 || m.PK[0].Column != "id" {
		t.Errorf("pk = %v", m.PK)
	}
	// A key over an omitted column can't drive keyset paging.
	if m := NewModel(apidef.Definition{Columns: []apidef.Column{{Name: "k", Type: "bytea"}, {Name: "v", Type: "text"}}, PrimaryKey: []string{"k"}}); m.PK != nil {
		t.Errorf("pk over an omitted column = %v", m.PK)
	}
}
