// Package gql generates and serves a generated API's GraphQL endpoint (ADR 0100;
// docs/design-v0.md §3, accepted as written): one schema per API, built from the stored table
// snapshot (internal/apidef), read-only, with depth, complexity, alias, filter and page-size limits
// checked on the parsed query before any SQL runs.
//
// Parsing and validation are vektah/gqlparser's, which is gqlgen's own parser and validator and is
// actively maintained. Execution is this package's: the schema shape is fixed and small, so walking
// a validated query lets every request become one parameterized SQL statement selecting only the
// requested columns. Why not graphql-go/graphql: docs/decisions/0002-graphql-library.md.
//
// Not mounted on any route yet: generated endpoints are served on core's public routes (ADR 0101).
package gql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/projectbooth/booth-api/internal/table"
)

// argType is an operator's argument type in a GraphQL filter input.
func argType(o table.Op, s table.Scalar) string {
	switch o.Name {
	case "in":
		return "[" + string(s) + "!]"
	case "isNull":
		return "Boolean"
	}
	return string(s)
}

// SDL renders a model's GraphQL schema.
func SDL(m table.Model) string {
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

	desc(&b, "", fmt.Sprintf("Read-only API over %q (table %s.%s).", m.Def.DatasetName, m.Def.Table.Schema, m.Def.Table.Name))
	b.WriteString("type Query {\n")
	desc(&b, "  ", "Rows matching `where`, in `orderBy` order. `first` is capped by the server; asking for more is an error.")
	b.WriteString("  rows(where: RowFilter, orderBy: [RowOrder!], first: Int = 50, after: String): RowConnection!\n")
	if len(m.PK) == 1 {
		desc(&b, "  ", "One row by primary key, or null.")
		fmt.Fprintf(&b, "  row(%s: %s!): Row\n", m.PK[0].Name, m.PK[0].Scalar)
	}
	b.WriteString("}\n\n")

	b.WriteString("type Row {\n")
	for _, f := range m.Fields {
		desc(&b, "  ", f.Desc)
		fmt.Fprintf(&b, "  %s: %s\n", f.Name, outType(f))
	}
	b.WriteString("}\n\n")
	b.WriteString("type RowConnection {\n  nodes: [Row!]!\n  pageInfo: PageInfo!\n}\n\n")
	b.WriteString("type PageInfo {\n  hasNextPage: Boolean!\n  \"Pass as `after` to get the next page.\"\n  endCursor: String\n}\n\n")

	b.WriteString("input RowFilter {\n  and: [RowFilter!]\n  or: [RowFilter!]\n  not: RowFilter\n")
	filters := map[table.Scalar]bool{}
	for _, f := range m.Fields {
		if f.Filterable() {
			fmt.Fprintf(&b, "  %s: %sFilter\n", f.Name, f.Scalar)
			filters[f.Scalar] = true
		}
	}
	b.WriteString("}\n\n")
	var scalars []string
	for s := range filters {
		scalars = append(scalars, string(s))
	}
	sort.Strings(scalars)
	for _, s := range scalars {
		fmt.Fprintf(&b, "input %sFilter {\n", s)
		for _, op := range table.OpsFor(table.Scalar(s)) {
			fmt.Fprintf(&b, "  %s: %s\n", op.Name, argType(op, table.Scalar(s)))
		}
		b.WriteString("}\n\n")
	}

	var orderable []string
	for _, f := range m.Fields {
		if f.Orderable() {
			orderable = append(orderable, f.Name)
		}
	}
	if len(orderable) > 0 {
		b.WriteString("enum RowField {\n")
		for _, n := range orderable {
			fmt.Fprintf(&b, "  %s\n", n)
		}
		b.WriteString("}\n\nenum OrderDirection {\n  ASC\n  DESC\n}\n\n")
		b.WriteString("input RowOrder {\n  field: RowField!\n  direction: OrderDirection = ASC\n}\n")
	} else {
		// RowOrder must exist (rows takes it) but nothing can be ordered by: an input with no
		// usable value would be invalid SDL, so give it a direction only, which the executor
		// refuses with a clear message.
		b.WriteString("enum OrderDirection {\n  ASC\n  DESC\n}\n\n\"No column of this table can be ordered by.\"\ninput RowOrder {\n  direction: OrderDirection\n}\n")
	}
	return b.String()
}

func outType(f table.Field) string {
	t := string(f.Scalar)
	if f.List {
		// Array elements may be NULL in Postgres, so the element type stays nullable.
		t = "[" + t + "]"
	}
	if !f.Nullable {
		t += "!"
	}
	return t
}

func desc(b *strings.Builder, indent, s string) {
	if s == "" {
		return
	}
	fmt.Fprintf(b, "%s\"\"\"%s\"\"\"\n", indent, strings.ReplaceAll(s, `"""`, `\"""`))
}
