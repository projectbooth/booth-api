package apidef

import (
	"reflect"
	"strings"
	"testing"
)

func TestReconcile(t *testing.T) {
	table := []Column{{Name: "id", Type: "bigint"}, {Name: "amount", Type: "numeric(12,2)", Nullable: true}}

	got, mm := Reconcile(nil, table)
	if mm != nil || !reflect.DeepEqual(got, table) {
		t.Errorf("no catalog schema: %v %v", got, mm)
	}

	got, mm = Reconcile([]CatalogColumn{{Name: "amount", Description: "in CAD"}, {Name: "id"}}, table)
	if mm != nil || got[1].Description != "in CAD" || got[0].Type != "bigint" {
		t.Errorf("matching schema: %+v %v", got, mm)
	}
	if table[1].Description != "" {
		t.Error("Reconcile modified its input")
	}

	_, mm = Reconcile([]CatalogColumn{{Name: "id"}, {Name: "currency"}}, table)
	want := []Mismatch{{"amount", ProblemNotInCatalog}, {"currency", ProblemNotInTable}}
	if !reflect.DeepEqual(mm, want) {
		t.Errorf("mismatches = %v, want %v", mm, want)
	}
	if msg := (&MismatchError{mm}).Error(); !strings.Contains(msg, "currency (in the catalog but not in the table)") {
		t.Errorf("error = %q", msg)
	}
}

func TestSlug(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		want string
	}{
		{"Orders", 1, "orders"},
		{"  Q3 Sales — EMEA!! ", 1, "q3-sales-emea"},
		{"Orders", 2, "orders-2"},
		{"日本", 1, "dataset"},
		{strings.Repeat("a", 100), 1, strings.Repeat("a", MaxSlug)},
		{strings.Repeat("a", 100), 12, strings.Repeat("a", MaxSlug-3) + "-12"},
		{strings.Repeat("ab-", 30), 1, strings.TrimRight(strings.Repeat("ab-", 30)[:MaxSlug], "-")},
	} {
		if got := Slug(tc.name, tc.n); got != tc.want {
			t.Errorf("Slug(%q, %d) = %q, want %q", tc.name, tc.n, got, tc.want)
		}
		if got := Slug(tc.name, tc.n); len(got) > MaxSlug {
			t.Errorf("Slug(%q, %d) is %d long", tc.name, tc.n, len(got))
		}
	}
}
