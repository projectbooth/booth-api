package gql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/validator"

	"github.com/projectbooth/booth-api/internal/table"
)

// Limits are the shared table limits plus GraphQL's own. Correctness limits, not quotas (ADR 0100
// item 4). Defaults are docs/design-v0.md §3's.
type Limits struct {
	table.Limits
	// MaxDepth bounds field nesting outside introspection; a legitimate data query is about four
	// deep (rows > nodes > field, pageInfo). Introspection gets its own, larger bound: the standard
	// introspection query nests ofType about eight levels below __schema.
	MaxDepth              int
	MaxIntrospectionDepth int
	// MaxComplexity bounds the sum over root fields of first × (selected row fields + 1).
	MaxComplexity int
	// MaxRootFields bounds aliased root fields (each `rows` is one SQL statement).
	MaxRootFields int
	MaxQueryBytes int
}

// DefaultLimits are the defaults the chart will expose once the endpoint is mounted.
var DefaultLimits = Limits{
	Limits: table.DefaultLimits, MaxDepth: 8, MaxIntrospectionDepth: 15, MaxComplexity: 10000,
	MaxRootFields: 5, MaxQueryBytes: 16 << 10,
}

// ErrNoFields: none of the table's columns has a type this API can serve.
var ErrNoFields = errors.New("none of the table's columns has a type the API can serve")

// Engine serves one generated API's GraphQL schema.
type Engine struct {
	Model  table.Model
	Schema *ast.Schema
	SDL    string
	Limits Limits
	// OnInternalError, if set, receives errors the caller only sees as "internal error" (for
	// logging; the HTTP layer sets it once the endpoint is mounted).
	OnInternalError func(error)
}

// New builds the engine for a definition. The SDL is generated, so a failure here is a bug in this
// package, not bad input; it is still returned rather than panicking.
func New(m table.Model, l Limits) (*Engine, error) {
	if len(m.Fields) == 0 {
		return nil, ErrNoFields
	}
	sdl := SDL(m)
	s, err := gqlparser.LoadSchema(&ast.Source{Name: "booth-api:" + m.Def.Slug, Input: sdl})
	if err != nil {
		return nil, fmt.Errorf("generated schema for %q is invalid: %w", m.Def.Slug, err)
	}
	return &Engine{Model: m, Schema: s, SDL: sdl, Limits: l}, nil
}

// Request is a GraphQL-over-HTTP request body.
type Request struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
}

// Response is a GraphQL response. Data is absent (not null) when the request failed before
// execution, per the spec.
type Response struct {
	Data   json.RawMessage `json:"data,omitempty"`
	Errors gqlerror.List   `json:"errors,omitempty"`
}

func fail(format string, args ...any) Response {
	return Response{Errors: gqlerror.List{gqlerror.Errorf(format, args...)}}
}

// Execute runs one request. Nothing touches the database until the query has parsed, validated
// and passed every limit.
func (e *Engine) Execute(ctx context.Context, db table.DB, req Request) Response {
	if len(req.Query) > e.Limits.MaxQueryBytes {
		return fail("query is larger than %d bytes", e.Limits.MaxQueryBytes)
	}
	doc, errs := gqlparser.LoadQuery(e.Schema, req.Query)
	if len(errs) > 0 {
		return Response{Errors: errs}
	}
	op := doc.Operations.ForName(req.OperationName)
	if op == nil {
		if req.OperationName == "" {
			return fail("the document has several operations; set operationName")
		}
		return fail("no operation named %q", req.OperationName)
	}
	if op.Operation != ast.Query {
		return fail("only queries are supported: this API is read-only") // validation already refuses; belt and braces
	}
	vars, verr := validator.VariableValues(e.Schema, op, req.Variables)
	if verr != nil {
		var ge *gqlerror.Error
		if errors.As(verr, &ge) {
			return Response{Errors: gqlerror.List{ge}}
		}
		return fail("%s", verr.Error())
	}

	x := &executor{e: e, vars: vars}
	if err := x.check(op); err != nil {
		return fail("%s", err.Error())
	}

	var out ordered
	err := table.WithTx(ctx, db, e.Limits.StatementTimeout, func(tx pgx.Tx) error {
		x.tx = tx
		var err error
		out, err = x.selectObject(ctx, op.SelectionSet, queryObj{})
		return err
	})
	if err != nil {
		var te *table.Error
		if errors.As(err, &te) {
			return fail("%s", te.Msg)
		}
		if e.OnInternalError != nil {
			e.OnInternalError(err)
		}
		return fail("internal error") // never echo a database error to the caller
	}
	data, merr := json.Marshal(out)
	if merr != nil {
		return fail("internal error")
	}
	return Response{Data: data}
}

// ---- field collection -------------------------------------------------------------------------

type executor struct {
	e    *Engine
	vars map[string]any
	tx   pgx.Tx
}

// collected is one response key's merged fields: the spec's CollectFields, with @skip/@include
// applied and same-key fields' selection sets merged (validation guarantees they're compatible).
type collected struct {
	key    string
	fields []*ast.Field
}

func (c collected) first() *ast.Field { return c.fields[0] }

func (c collected) subSelections() ast.SelectionSet {
	var ss ast.SelectionSet
	for _, f := range c.fields {
		ss = append(ss, f.SelectionSet...)
	}
	return ss
}

// collect flattens fragments; there are no interfaces or unions in this schema, so every
// validated fragment's type condition applies to the object it is spread into.
func (x *executor) collect(ss ast.SelectionSet) []collected {
	var out []collected
	index := map[string]int{}
	var walk func(ast.SelectionSet)
	walk = func(ss ast.SelectionSet) {
		for _, s := range ss {
			switch n := s.(type) {
			case *ast.Field:
				if !x.included(n.Directives) {
					continue
				}
				key := n.Alias
				if key == "" {
					key = n.Name
				}
				if i, ok := index[key]; ok {
					out[i].fields = append(out[i].fields, n)
					continue
				}
				index[key] = len(out)
				out = append(out, collected{key: key, fields: []*ast.Field{n}})
			case *ast.FragmentSpread:
				if x.included(n.Directives) && n.Definition != nil {
					walk(n.Definition.SelectionSet)
				}
			case *ast.InlineFragment:
				if x.included(n.Directives) {
					walk(n.SelectionSet)
				}
			}
		}
	}
	walk(ss)
	return out
}

func (x *executor) included(ds ast.DirectiveList) bool {
	if d := ds.ForName("skip"); d != nil {
		if v, _ := d.ArgumentMap(x.vars)["if"].(bool); v {
			return false
		}
	}
	if d := ds.ForName("include"); d != nil {
		if v, _ := d.ArgumentMap(x.vars)["if"].(bool); !v {
			return false
		}
	}
	return true
}

func (x *executor) args(c collected) map[string]any { return c.first().ArgumentMap(x.vars) }

// ---- limits -----------------------------------------------------------------------------------

func (x *executor) check(op *ast.OperationDefinition) error {
	l := x.e.Limits
	roots := 0
	cost := 0
	for _, c := range x.collect(op.SelectionSet) {
		name := c.first().Name
		if strings.HasPrefix(name, "__") {
			if d := x.depth(c.subSelections(), 2); d > l.MaxIntrospectionDepth {
				return fmt.Errorf("introspection query is nested %d deep; the limit is %d", d, l.MaxIntrospectionDepth)
			}
			continue
		}
		roots++
		if d := x.depth(c.subSelections(), 2); d > l.MaxDepth {
			return fmt.Errorf("query is nested %d deep; the limit is %d", d, l.MaxDepth)
		}
		switch name {
		case "rows":
			first, err := x.first(c)
			if err != nil {
				return err
			}
			if err := table.CheckFilter(x.args(c)["where"], x.e.Limits.Limits); err != nil {
				return err
			}
			nodes, page := 0, 0
			for _, sub := range x.collect(c.subSelections()) {
				switch sub.first().Name {
				case "nodes":
					nodes += len(x.collect(sub.subSelections()))
				case "pageInfo":
					page++
				}
			}
			cost += first*(nodes+1) + page
		case "row":
			cost += len(x.collect(c.subSelections())) + 1
		}
	}
	if roots > l.MaxRootFields {
		return fmt.Errorf("query has %d root fields; the limit is %d", roots, l.MaxRootFields)
	}
	if cost > l.MaxComplexity {
		return fmt.Errorf("query cost %d is over the limit of %d (cost is first × (selected row fields + 1), summed over root fields); ask for fewer rows or fields", cost, l.MaxComplexity)
	}
	return nil
}

// depth is the deepest field nesting in ss, counting ss's own fields as level `level`.
func (x *executor) depth(ss ast.SelectionSet, level int) int {
	deepest := level - 1
	for _, c := range x.collect(ss) {
		d := level
		if sub := c.subSelections(); len(sub) > 0 {
			d = x.depth(sub, level+1)
		}
		deepest = max(deepest, d)
	}
	return deepest
}

// first returns the effective page size, refusing (not clamping) one over the limit.
func (x *executor) first(c collected) (int, error) {
	first := x.e.Limits.DefaultFirst
	if v, ok := x.args(c)["first"]; ok && v != nil {
		n, ok := asInt(v)
		if !ok {
			return 0, table.Errorf("first must be an integer")
		}
		first = n
	}
	return first, table.CheckFirst(first, x.e.Limits.Limits)
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// ---- objects ----------------------------------------------------------------------------------

// object is anything a selection set is resolved against.
type object interface {
	typeName() string
	resolve(ctx context.Context, x *executor, c collected) (any, error)
}

// list is a list of objects; plain values (scalars, []any of scalars, nil) pass through.
type list []object

func (x *executor) selectObject(ctx context.Context, ss ast.SelectionSet, o object) (ordered, error) {
	var out ordered
	for _, c := range x.collect(ss) {
		if c.first().Name == "__typename" {
			out = append(out, kv{c.key, o.typeName()})
			continue
		}
		v, err := o.resolve(ctx, x, c)
		if err != nil {
			return nil, err
		}
		v, err = x.complete(ctx, c, v)
		if err != nil {
			return nil, err
		}
		out = append(out, kv{c.key, v})
	}
	return out, nil
}

func (x *executor) complete(ctx context.Context, c collected, v any) (any, error) {
	switch t := v.(type) {
	case object:
		if t == nil {
			return nil, nil
		}
		return x.selectObject(ctx, c.subSelections(), t)
	case list:
		out := make([]any, len(t))
		for i, o := range t {
			r, err := x.selectObject(ctx, c.subSelections(), o)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

type queryObj struct{}

func (queryObj) typeName() string { return "Query" }

func (queryObj) resolve(ctx context.Context, x *executor, c collected) (any, error) {
	switch c.first().Name {
	case "rows":
		return x.rows(ctx, c)
	case "row":
		return x.row(ctx, c)
	case "__schema":
		return schemaObj{x.e.Schema}, nil
	case "__type":
		name, _ := x.args(c)["name"].(string)
		if d := x.e.Schema.Types[name]; d != nil {
			return typeObj{s: x.e.Schema, def: d}, nil
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unknown root field %q", c.first().Name)
}

// ordered is a JSON object that keeps key order (the spec asks responses to follow the query's).
type kv struct {
	k string
	v any
}

type ordered []kv

func (o ordered) MarshalJSON() ([]byte, error) {
	if o == nil {
		return []byte("{}"), nil
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.k)
		b.Write(k)
		b.WriteByte(':')
		v, err := json.Marshal(e.v)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
