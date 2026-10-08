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
	"regexp"
	"sort"
	"strings"

	"github.com/projectbooth/booth-api/internal/apidef"
)

// Scalar is a GraphQL scalar this package maps Postgres types onto (docs/design-v0.md §3's table).
type Scalar string

const (
	Int      Scalar = "Int"
	BigInt   Scalar = "BigInt"  // bigint, as a string: GraphQL Int is 32-bit, JSON loses precision past 2^53
	Decimal  Scalar = "Decimal" // numeric, as a string: the exact value
	Float    Scalar = "Float"
	Boolean  Scalar = "Boolean"
	String   Scalar = "String"
	ID       Scalar = "ID" // uuid
	Date     Scalar = "Date"
	DateTime Scalar = "DateTime"
	Time     Scalar = "Time"
	JSON     Scalar = "JSON"
)

// customScalars are declared in the generated SDL; the rest are GraphQL built-ins.
var customScalars = []Scalar{BigInt, Decimal, Date, DateTime, Time, JSON}

// Field is one exposed column.
type Field struct {
	// Name is the GraphQL field name; Column the real column name (quoted in SQL, never from input).
	Name   string
	Column string
	// PGType is the column's format_type, used as the cast for every bound value.
	PGType   string
	Scalar   Scalar
	List     bool
	Nullable bool
	Desc     string
}

// Filterable: comparisons are defined for every scalar but JSON, and not for arrays (v0).
func (f Field) Filterable() bool { return !f.List && f.Scalar != JSON }

// Orderable: keyset pagination compares cursor values with < and >, which NULLs break, so only
// filterable NOT NULL columns can be ordered by (docs/decisions/0003).
func (f Field) Orderable() bool { return f.Filterable() && !f.Nullable }

// Omitted is a column left out of the API because its type has no faithful mapping.
type Omitted struct {
	Column string `json:"column"`
	Type   string `json:"type"`
}

// Model is a definition mapped for GraphQL: exposed fields in column order, the primary key, and
// what was left out.
type Model struct {
	Def     apidef.Definition
	Fields  []Field
	ByName  map[string]Field
	PK      []Field // empty when the table has no primary key, or a key column was omitted
	Omitted []Omitted
}

var (
	reArrayDims = regexp.MustCompile(`(\[\])+$`)
	reTypmod    = regexp.MustCompile(`\(.*\)`)
)

// scalarFor maps a format_type rendering. ok is false for anything without a faithful mapping
// (bytea, money, intervals, geometric, network, range, composite, user enum types, ...); those are
// omitted rather than converted lossily.
func scalarFor(pgType string) (s Scalar, list, ok bool) {
	base := pgType
	if reArrayDims.MatchString(base) {
		if strings.Count(base, "[]") > 1 {
			return "", false, false // multi-dimensional arrays: no faithful nested-list shape
		}
		list = true
		base = reArrayDims.ReplaceAllString(base, "")
	}
	base = strings.TrimSpace(reTypmod.ReplaceAllString(base, ""))
	switch base {
	case "smallint", "integer":
		s = Int
	case "bigint":
		s = BigInt
	case "numeric":
		s = Decimal
	case "real", "double precision":
		s = Float
	case "boolean":
		s = Boolean
	case "text", "character varying", "character", "citext", "name":
		s = String
	case "uuid":
		s = ID
	case "date":
		s = Date
	case "timestamp without time zone", "timestamp with time zone":
		s = DateTime
	case "time without time zone":
		s = Time
	case "json", "jsonb":
		s = JSON
	default:
		return "", false, false
	}
	return s, list, true
}

var nameUnsafe = regexp.MustCompile(`[^_0-9A-Za-z]`)

// fieldName makes a column name a legal GraphQL name (`[_A-Za-z][_0-9A-Za-z]*`, not starting with
// `__`, which is reserved for introspection).
func fieldName(column string) string {
	n := nameUnsafe.ReplaceAllString(column, "_")
	if n == "" || (n[0] >= '0' && n[0] <= '9') {
		n = "_" + n
	}
	for strings.HasPrefix(n, "__") {
		n = n[1:]
		if n == "_" {
			n = "_col"
		}
	}
	return n
}

// reserved names a Row field may not take: the GraphQL meta-field and the RowFilter combinators
// (filter inputs share Row's field names, so a column called "and" would collide with them).
var reserved = map[string]bool{"__typename": true, "and": true, "or": true, "not": true}

// NewModel maps a definition. Deterministic: the same snapshot always yields the same names, so a
// stored snapshot fixes the schema without storing the mapping separately.
func NewModel(def apidef.Definition) Model {
	m := Model{Def: def, ByName: map[string]Field{}}
	byColumn := map[string]Field{}
	for _, c := range def.Columns {
		s, list, ok := scalarFor(c.Type)
		if !ok {
			m.Omitted = append(m.Omitted, Omitted{Column: c.Name, Type: c.Type})
			continue
		}
		name := fieldName(c.Name)
		if reserved[name] {
			name += "_"
		}
		for i, base := 2, name; m.ByName[name].Name != ""; i++ {
			name = fmt.Sprintf("%s_%d", base, i)
		}
		f := Field{Name: name, Column: c.Name, PGType: c.Type, Scalar: s, List: list, Nullable: c.Nullable, Desc: c.Description}
		m.Fields = append(m.Fields, f)
		m.ByName[name] = f
		byColumn[c.Name] = f
	}
	for _, col := range def.PrimaryKey {
		f, ok := byColumn[col]
		if !ok || !f.Orderable() {
			m.PK = nil // a key column that isn't exposed or comparable can't drive keyset paging
			break
		}
		m.PK = append(m.PK, f)
	}
	return m
}

// SDL renders the schema.
func (m Model) SDL() string {
	var b strings.Builder
	used := map[Scalar]bool{}
	for _, f := range m.Fields {
		used[f.Scalar] = true
	}
	for _, s := range customScalars {
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
	filters := map[Scalar]bool{}
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
		for _, op := range opsFor(Scalar(s)) {
			fmt.Fprintf(&b, "  %s: %s\n", op.name, op.argType(Scalar(s)))
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

func outType(f Field) string {
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

// op is one filter operator.
type op struct {
	name string
	sql  string // comparison operator; "" for the special cases (in, isNull, startsWith)
}

func (o op) argType(s Scalar) string {
	switch o.name {
	case "in":
		return "[" + string(s) + "!]"
	case "isNull":
		return "Boolean"
	}
	return string(s)
}

var (
	opsEquality = []op{{"eq", "="}, {"neq", "<>"}, {"in", ""}, {"isNull", ""}}
	opsOrdered  = append(append([]op{}, opsEquality...), op{"lt", "<"}, op{"lte", "<="}, op{"gt", ">"}, op{"gte", ">="})
)

func opsFor(s Scalar) []op {
	switch s {
	case Boolean, ID:
		return opsEquality
	case String:
		// startsWith is an anchored prefix match: the value is escaped and only a trailing
		// wildcard is added, so it can't be turned into an unanchored scan.
		return append(append([]op{}, opsOrdered...), op{"startsWith", ""})
	}
	return opsOrdered
}
