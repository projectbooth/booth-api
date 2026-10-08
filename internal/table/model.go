// Package table is the query layer both generated protocols share (ADR 0100): the GraphQL
// endpoint (internal/gql) and the REST one (internal/rest) map a definition's snapshot the same way,
// compile filters the same way, page with the same cursors and are bound by the same limits, so the
// two never disagree about what a dataset's API returns.
//
// Every identifier in generated SQL comes from the stored snapshot (internal/apidef), quoted with
// pgx.Identifier; every value from a request is a bound parameter cast to the column's own type
// (`$n::<format_type>`), checked first against the scalar's documented format.
package table

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/projectbooth/booth-api/internal/apidef"
)

// Scalar is the API type a Postgres type maps onto (docs/design-v0.md §3's table). The names are
// GraphQL's; REST serializes values identically.
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

// CustomScalars are declared in a generated GraphQL schema; the rest are GraphQL built-ins.
var CustomScalars = []Scalar{BigInt, Decimal, Date, DateTime, Time, JSON}

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

// Op is one filter operator, shared by both protocols: GraphQL's per-scalar filter inputs and
// REST's `field[op]=value` query parameters.
type Op struct {
	Name string
	SQL  string // comparison operator; "" for the special cases (in, isNull, startsWith)
}

var (
	opsEquality = []Op{{"eq", "="}, {"neq", "<>"}, {"in", ""}, {"isNull", ""}}
	opsOrdered  = append(append([]Op{}, opsEquality...), Op{"lt", "<"}, Op{"lte", "<="}, Op{"gt", ">"}, Op{"gte", ">="})
)

// OpsFor lists the operators a scalar supports.
func OpsFor(s Scalar) []Op {
	switch s {
	case Boolean, ID:
		return opsEquality
	case String:
		// startsWith is an anchored prefix match: the value is escaped and only a trailing
		// wildcard is added, so it can't be turned into an unanchored scan.
		return append(append([]Op{}, opsOrdered...), Op{"startsWith", ""})
	}
	return opsOrdered
}
