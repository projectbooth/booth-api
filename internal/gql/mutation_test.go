package gql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/table"
)

func writeModel() table.Model {
	return table.NewModel(apidef.Definition{
		Slug: "entries", DatasetName: "Entries", Table: apidef.TableRef{Schema: "finance", Name: "entries"},
		Columns: []apidef.Column{
			{Name: "id", Type: "bigint", Identity: "d"},
			{Name: "amount", Type: "numeric(12,2)"},
			{Name: "note", Type: "text", Nullable: true},
			{Name: "booked", Type: "date", HasDefault: true},
		},
		PrimaryKey: []string{"id"},
	})
}

// The write schema lists only the enabled operations and only the writable columns.
func TestWriteSDL(t *testing.T) {
	m := writeModel()
	got := WriteSDL(m, &apidef.WriteSettings{Operations: []string{"insert", "update"}, Columns: []string{"amount", "note"}})
	want := `scalar BigInt
scalar Decimal
scalar Date

"""Write API over "Entries" (table finance.entries). Only mutations are served here."""
type Query {
  "Always null: this endpoint takes mutations only."
  _writeOnly: Boolean
}

type Mutation {
  """Insert one row. Columns not given take their defaults."""
  insertRow(data: RowInsert!, idempotencyKey: String): RowWrite!
  """Set columns of the row with this key. With expectedVersion, only if the row is still at that version."""
  updateRow(key: RowKey!, set: RowPatch!, expectedVersion: String, idempotencyKey: String): RowWrite!
}

type Row {
  id: BigInt!
  amount: Decimal!
  note: String
  booked: Date!
}

type RowWrite {
  row: Row!
  "The row's version after the write: pass as expectedVersion to update or delete only an unchanged row."
  version: String!
}

input RowInsert {
  amount: Decimal
  note: String
}

input RowPatch {
  amount: Decimal
  note: String
}

input RowKey {
  id: BigInt!
}
`
	if got != want {
		t.Errorf("SDL:\n%s\nwant:\n%s", got, want)
	}
	del := WriteSDL(m, &apidef.WriteSettings{Operations: []string{"delete"}, Columns: []string{}})
	if strings.Contains(del, "insertRow") || strings.Contains(del, "updateRow") || !strings.Contains(del, "deleteRow(key: RowKey!") || strings.Contains(del, "RowInsert") {
		t.Errorf("delete-only SDL:\n%s", del)
	}
	if _, err := NewWrite(m, &apidef.WriteSettings{Operations: []string{"delete"}, Columns: []string{}}, DefaultLimits); err != nil {
		t.Errorf("delete-only schema doesn't load: %v", err)
	}
}

func TestParseMutation(t *testing.T) {
	e, err := NewWrite(writeModel(), &apidef.WriteSettings{Operations: []string{"insert", "update"}, Columns: []string{"amount", "note"}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	mu, resp := e.Parse(Request{
		Query:     `mutation($d: RowInsert!) { insertRow(data: $d, idempotencyKey: "k1") { row { id amount } v: version } }`,
		Variables: map[string]any{"d": map[string]any{"amount": "12.50", "note": nil}},
	})
	if resp != nil {
		t.Fatalf("parse: %+v", resp.Errors)
	}
	if mu.Request.Op != "insert" || mu.IdempotencyKey != "k1" || mu.Request.Data["amount"] != "12.50" {
		t.Errorf("mutation = %+v", mu.Request)
	}
	if v, ok := mu.Request.Data["note"]; !ok || v != nil {
		t.Errorf("an explicit null must reach the write as null: %+v", mu.Request.Data)
	}
	out := mu.Respond(context.Background(), table.Row{"id": json.RawMessage(`"7"`), "amount": json.RawMessage(`"12.50"`), "note": json.RawMessage(`null`)}, "abc")
	if string(out.Data) != `{"insertRow":{"row":{"id":"7","amount":"12.50"},"v":"abc"}}` {
		t.Errorf("response = %s", out.Data)
	}

	mu, resp = e.Parse(Request{Query: `mutation { updateRow(key: {id: "7"}, set: {note: "x"}, expectedVersion: "abc") { version } }`})
	if resp != nil || mu.Request.Op != "update" || mu.Request.Key["id"] != "7" || mu.Request.ExpectedVersion != "abc" {
		t.Errorf("update = %+v, %+v", mu, resp)
	}

	for name, q := range map[string]string{
		"a query":              `{ _writeOnly }`,
		"two mutations":        `mutation { a: insertRow(data: {amount: "1"}) { version } b: insertRow(data: {amount: "2"}) { version } }`,
		"a column not in it":   `mutation { insertRow(data: {booked: "2026-01-01"}) { version } }`,
		"delete isn't enabled": `mutation { deleteRow(key: {id: "1"}) { version } }`,
	} {
		if _, resp := e.Parse(Request{Query: q}); resp == nil || len(resp.Errors) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWriteFailureShape(t *testing.T) {
	r := WriteFailure(&table.WriteError{Status: 422, Code: "required", Msg: "the row isn't valid", Fields: []table.FieldProblem{{Field: "amount", Problem: "required"}}})
	b, _ := json.Marshal(r)
	if string(b) != `{"errors":[{"message":"the row isn't valid","extensions":{"code":"required","fields":[{"field":"amount","problem":"required"}]}}]}` {
		t.Errorf("failure = %s", b)
	}
}
