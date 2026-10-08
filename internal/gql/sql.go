package gql

import (
	"context"
	"encoding/json"

	"github.com/projectbooth/booth-api/internal/table"
)

// The data path: `rows` and `row` resolve through internal/table, which REST shares, so both
// protocols compile the same filter to the same SQL and page with interchangeable cursors.

type rowObj table.Row

func (rowObj) typeName() string { return "Row" }

func (r rowObj) resolve(_ context.Context, _ *executor, c collected) (any, error) {
	return json.RawMessage(r[c.first().Name]), nil
}

type pageInfoObj struct {
	hasNext bool
	end     *string
}

func (pageInfoObj) typeName() string { return "PageInfo" }

func (p pageInfoObj) resolve(_ context.Context, _ *executor, c collected) (any, error) {
	if c.first().Name == "hasNextPage" {
		return p.hasNext, nil
	}
	if p.end == nil {
		return nil, nil
	}
	return *p.end, nil
}

type connectionObj struct {
	nodes list
	page  pageInfoObj
}

func (connectionObj) typeName() string { return "RowConnection" }

func (c connectionObj) resolve(_ context.Context, _ *executor, f collected) (any, error) {
	if f.first().Name == "nodes" {
		if c.nodes == nil {
			return []any{}, nil
		}
		return c.nodes, nil
	}
	return c.page, nil
}

// selectedRowFields is the union of Row fields selected under every given selection.
func (x *executor) selectedRowFields(ss []collected) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range ss {
		for _, f := range x.collect(c.subSelections()) {
			name := f.first().Name
			if _, ok := x.e.Model.ByName[name]; ok && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// orderBy converts GraphQL's [RowOrder!] into table.OrderBy.
func orderBy(v any) ([]table.OrderBy, error) {
	items, _ := v.([]any)
	if m, ok := v.(map[string]any); ok { // a single RowOrder is coerced to a list by the spec, but be lenient
		items = []any{m}
	}
	var out []table.OrderBy
	for _, it := range items {
		m, _ := it.(map[string]any)
		name, _ := m["field"].(string)
		if name == "" {
			return nil, table.Errorf("no column of this table can be ordered by")
		}
		dir, _ := m["direction"].(string)
		out = append(out, table.OrderBy{Field: name, Desc: dir == "DESC"})
	}
	return out, nil
}

func (x *executor) rows(ctx context.Context, c collected) (any, error) {
	first, err := x.first(c)
	if err != nil {
		return nil, err
	}
	args := x.args(c)
	order, err := orderBy(args["orderBy"])
	if err != nil {
		return nil, err
	}
	var nodes []collected
	for _, sub := range x.collect(c.subSelections()) {
		if sub.first().Name == "nodes" {
			nodes = append(nodes, sub)
		}
	}
	after, _ := args["after"].(string)
	res, err := table.Fetch(ctx, x.tx, x.e.Model, table.Page{
		Where: args["where"], OrderBy: order, First: first, After: after, Fields: x.selectedRowFields(nodes),
	}, x.e.Limits.Limits)
	if err != nil {
		return nil, err
	}
	out := make(list, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = rowObj(r)
	}
	return connectionObj{nodes: out, page: pageInfoObj{hasNext: res.HasNext, end: res.EndCursor}}, nil
}

func (x *executor) row(ctx context.Context, c collected) (any, error) {
	if len(x.e.Model.PK) != 1 {
		return nil, table.Errorf("this table has no single-column primary key")
	}
	r, found, err := table.Get(ctx, x.tx, x.e.Model, x.args(c)[x.e.Model.PK[0].Name], x.selectedRowFields([]collected{c}))
	if err != nil || !found {
		return nil, err
	}
	return rowObj(r), nil
}
