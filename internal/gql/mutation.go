package gql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/validator"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/table"
)

// The write schema (ADR 0117), served only on the app write route: one mutation field per
// request, for the operations the API's settings turn on, with input types listing only the
// writable columns. Reads aren't served there (an app reads through its own read path), so Query
// has a single placeholder field and a query operation is refused.
//
//	insertRow(data: RowInsert!, idempotencyKey: String): RowWrite!
//	updateRow(key: RowKey!, set: RowPatch!, expectedVersion: String, idempotencyKey: String): RowWrite!
//	deleteRow(key: RowKey!, expectedVersion: String, idempotencyKey: String): RowWrite!

// Mutation field names.
const (
	FieldInsert = "insertRow"
	FieldUpdate = "updateRow"
	FieldDelete = "deleteRow"
)

// WriteEngine serves one API's write schema.
type WriteEngine struct {
	Model  table.Model
	Schema *ast.Schema
	SDL    string
	Limits Limits
}

// NewWrite builds the write engine for an API's model and write settings.
func NewWrite(m table.Model, w *apidef.WriteSettings, l Limits) (*WriteEngine, error) {
	if w == nil {
		return nil, errors.New("writes are off for this API")
	}
	if len(m.Fields) == 0 {
		return nil, ErrNoFields
	}
	sdl := WriteSDL(m, w)
	s, err := gqlparser.LoadSchema(&ast.Source{Name: "booth-api-write:" + m.Def.Slug, Input: sdl})
	if err != nil {
		return nil, fmt.Errorf("generated write schema for %q is invalid: %w", m.Def.Slug, err)
	}
	return &WriteEngine{Model: m, Schema: s, SDL: sdl, Limits: l}, nil
}

// writableFields is the model's fields among the settings' columns, in column order.
func writableFields(m table.Model, w *apidef.WriteSettings) []table.Field {
	var out []table.Field
	for _, f := range m.Fields {
		if slices.Contains(w.Columns, f.Column) {
			out = append(out, f)
		}
	}
	return out
}

// WriteSDL renders the write schema.
func WriteSDL(m table.Model, w *apidef.WriteSettings) string {
	var b strings.Builder
	used := map[table.Scalar]bool{}
	for _, f := range m.Fields {
		used[f.Scalar] = true
	}
	for _, s := range table.CustomScalars {
		if used[s] {
			fmt.Fprintf(&b, "scalar %s\n", s)
		}
	}
	b.WriteString("\n")
	desc(&b, "", fmt.Sprintf("Write API over %q (table %s.%s). Only mutations are served here.", m.Def.DatasetName, m.Def.Table.Schema, m.Def.Table.Name))
	b.WriteString("type Query {\n  \"Always null: this endpoint takes mutations only.\"\n  _writeOnly: Boolean\n}\n\n")

	cols := writableFields(m, w)
	keyed := len(m.PK) > 0
	b.WriteString("type Mutation {\n")
	if w.Allows(apidef.OpInsert) && len(cols) > 0 {
		desc(&b, "  ", "Insert one row. Columns not given take their defaults.")
		b.WriteString("  " + FieldInsert + "(data: RowInsert!, idempotencyKey: String): RowWrite!\n")
	}
	if w.Allows(apidef.OpUpdate) && keyed && len(cols) > 0 {
		desc(&b, "  ", "Set columns of the row with this key. With expectedVersion, only if the row is still at that version.")
		b.WriteString("  " + FieldUpdate + "(key: RowKey!, set: RowPatch!, expectedVersion: String, idempotencyKey: String): RowWrite!\n")
	}
	if w.Allows(apidef.OpDelete) && keyed {
		desc(&b, "  ", "Delete the row with this key and return it. With expectedVersion, only if the row is still at that version.")
		b.WriteString("  " + FieldDelete + "(key: RowKey!, expectedVersion: String, idempotencyKey: String): RowWrite!\n")
	}
	b.WriteString("}\n\n")

	b.WriteString("type Row {\n")
	for _, f := range m.Fields {
		desc(&b, "  ", f.Desc)
		fmt.Fprintf(&b, "  %s: %s\n", f.Name, outType(f))
	}
	b.WriteString("}\n\n")
	b.WriteString("type RowWrite {\n  row: Row!\n  \"The row's version after the write: pass as expectedVersion to update or delete only an unchanged row.\"\n  version: String!\n}\n\n")
	if len(cols) > 0 {
		for _, name := range []string{"RowInsert", "RowPatch"} {
			fmt.Fprintf(&b, "input %s {\n", name)
			for _, f := range cols {
				fmt.Fprintf(&b, "  %s: %s\n", f.Name, f.Scalar)
			}
			b.WriteString("}\n\n")
		}
	}
	if keyed {
		b.WriteString("input RowKey {\n")
		for _, f := range m.PK {
			fmt.Fprintf(&b, "  %s: %s!\n", f.Name, f.Scalar)
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// Mutation is a parsed and validated write, ready to run.
type Mutation struct {
	Request        table.WriteRequest
	IdempotencyKey string
	x              *executor
	field          collected
}

// Parse validates a request against the write schema and extracts its one mutation. A non-nil
// Response is the error to send instead.
func (e *WriteEngine) Parse(req Request) (*Mutation, *Response) {
	failed := func(format string, args ...any) (*Mutation, *Response) {
		r := fail(format, args...)
		return nil, &r
	}
	if len(req.Query) > e.Limits.MaxQueryBytes {
		return failed("query is larger than %d bytes", e.Limits.MaxQueryBytes)
	}
	doc, errs := gqlparser.LoadQuery(e.Schema, req.Query)
	if len(errs) > 0 {
		return nil, &Response{Errors: errs}
	}
	op := doc.Operations.ForName(req.OperationName)
	if op == nil {
		if req.OperationName == "" {
			return failed("the document has several operations; set operationName")
		}
		return failed("no operation named %q", req.OperationName)
	}
	if op.Operation != ast.Mutation {
		return failed("only mutations are served here; an app reads through its own read path")
	}
	vars, verr := validator.VariableValues(e.Schema, op, req.Variables)
	if verr != nil {
		var ge *gqlerror.Error
		if errors.As(verr, &ge) {
			return nil, &Response{Errors: gqlerror.List{ge}}
		}
		return failed("%s", verr.Error())
	}
	x := &executor{vars: vars, e: &Engine{Model: e.Model, Schema: e.Schema, Limits: e.Limits}}
	roots := x.collect(op.SelectionSet)
	if len(roots) != 1 || strings.HasPrefix(roots[0].first().Name, "__") {
		return failed("send exactly one mutation field per request (one row per request)")
	}
	c := roots[0]
	if d := x.depth(c.subSelections(), 2); d > e.Limits.MaxDepth {
		return failed("query is nested %d deep; the limit is %d", d, e.Limits.MaxDepth)
	}
	args := x.args(c)
	mu := &Mutation{x: x, field: c}
	if k, ok := args["idempotencyKey"].(string); ok {
		mu.IdempotencyKey = k
	}
	if v, ok := args["expectedVersion"].(string); ok {
		mu.Request.ExpectedVersion = v
	}
	asMap := func(v any) map[string]any {
		m, _ := v.(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		return m
	}
	switch c.first().Name {
	case FieldInsert:
		mu.Request.Op, mu.Request.Data = apidef.OpInsert, asMap(args["data"])
	case FieldUpdate:
		mu.Request.Op, mu.Request.Key, mu.Request.Data = apidef.OpUpdate, asMap(args["key"]), asMap(args["set"])
	case FieldDelete:
		mu.Request.Op, mu.Request.Key = apidef.OpDelete, asMap(args["key"])
	default:
		return failed("unknown mutation %q", c.first().Name)
	}
	return mu, nil
}

type writeObj struct {
	row     table.Row
	version string
}

func (writeObj) typeName() string { return "RowWrite" }

func (w writeObj) resolve(_ context.Context, _ *executor, c collected) (any, error) {
	if c.first().Name == "version" {
		return w.version, nil
	}
	return rowObj(w.row), nil
}

type mutationObj struct{ result writeObj }

func (mutationObj) typeName() string { return "Mutation" }

func (m mutationObj) resolve(context.Context, *executor, collected) (any, error) {
	return m.result, nil
}

// Respond renders the written row through the mutation's own selection set.
func (mu *Mutation) Respond(ctx context.Context, row table.Row, version string) Response {
	out, err := mu.x.selectObject(ctx, ast.SelectionSet{mu.field.first()}, mutationObj{writeObj{row: row, version: version}})
	if err != nil {
		return fail("internal error")
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fail("internal error")
	}
	return Response{Data: data}
}

// WriteFailure is a refused write in GraphQL's shape: the message, with the code, the per-field
// problems and the constraint name (never values) under extensions.
func WriteFailure(we *table.WriteError) Response {
	ext := map[string]any{"code": we.Code}
	if len(we.Fields) > 0 {
		ext["fields"] = we.Fields
	}
	if we.Constraint != "" {
		ext["constraint"] = we.Constraint
	}
	return Response{Errors: gqlerror.List{{Message: we.Msg, Extensions: ext}}}
}
