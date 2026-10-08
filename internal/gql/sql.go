package gql

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Every identifier in generated SQL comes from the stored snapshot (internal/apidef), quoted with
// pgx.Identifier; every value from the request is a bound parameter cast to the column's own type
// (`$n::<format_type>`), so a malformed value is a Postgres cast error, never SQL.

func quote(parts ...string) string { return pgx.Identifier(parts).Sanitize() }

// sqlQuery accumulates a statement's arguments.
type sqlQuery struct{ args []any }

func (q *sqlQuery) bind(v any) string {
	q.args = append(q.args, v)
	return "$" + strconv.Itoa(len(q.args))
}

// bindAs binds v as text cast to the column's type.
func (q *sqlQuery) bindAs(f Field, v any) (string, error) {
	s, err := literal(f, v)
	if err != nil {
		return "", err
	}
	return q.bind(s) + "::" + f.PGType, nil
}

// Input formats for the custom scalars. Checked here rather than left to Postgres's cast, which
// also accepts things like 'yesterday', 'now', 'infinity' and '1e3' for these types: an API
// documented as ISO 8601 and exact decimals shouldn't quietly take them.
var scalarFormats = map[Scalar]*regexp.Regexp{
	BigInt:   regexp.MustCompile(`^-?[0-9]{1,19}$`),
	Decimal:  regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`), // length is bounded by MaxQueryBytes
	Date:     regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`),
	DateTime: regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?(Z|[+-][0-9]{2}:[0-9]{2})?$`),
	Time:     regexp.MustCompile(`^[0-9]{2}:[0-9]{2}(:[0-9]{2}(\.[0-9]{1,6})?)?$`),
}

// literal renders a GraphQL input value as the text Postgres will cast. Custom scalars (BigInt,
// Decimal, dates) arrive as strings or numbers; both are accepted, in the formats above.
func literal(f Field, v any) (string, error) {
	s, err := rawLiteral(f, v)
	if err != nil {
		return "", err
	}
	if re := scalarFormats[f.Scalar]; re != nil && !re.MatchString(s) {
		return "", userErr("%s: %q isn't a valid %s", f.Name, s, f.Scalar)
	}
	return s, nil
}

func rawLiteral(f Field, v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	case json.Number:
		return t.String(), nil
	case nil:
		return "", userErr("%s: null isn't a value to compare with; use isNull", f.Name)
	}
	return "", userErr("%s: unsupported value %v", f.Name, v)
}

// where compiles a RowFilter.
func (x *executor) where(q *sqlQuery, v any) (string, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return "TRUE", nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable SQL text for the same filter
	var parts []string
	for _, k := range keys {
		sub := m[k]
		switch k {
		case "and", "or":
			items, _ := sub.([]any)
			if len(items) == 0 {
				continue
			}
			var ps []string
			for _, it := range items {
				p, err := x.where(q, it)
				if err != nil {
					return "", err
				}
				ps = append(ps, "("+p+")")
			}
			joiner := " AND "
			if k == "or" {
				joiner = " OR "
			}
			parts = append(parts, "("+strings.Join(ps, joiner)+")")
		case "not":
			if sub == nil {
				continue
			}
			p, err := x.where(q, sub)
			if err != nil {
				return "", err
			}
			parts = append(parts, "NOT ("+p+")")
		default:
			f, ok := x.e.Model.ByName[k]
			if !ok || !f.Filterable() {
				return "", userErr("%s can't be filtered on", k) // validation should have caught it
			}
			ops, _ := sub.(map[string]any)
			p, err := x.compare(q, f, ops)
			if err != nil {
				return "", err
			}
			parts = append(parts, p...)
		}
	}
	if len(parts) == 0 {
		return "TRUE", nil
	}
	return strings.Join(parts, " AND "), nil
}

func (x *executor) compare(q *sqlQuery, f Field, ops map[string]any) ([]string, error) {
	col := quote(f.Column)
	names := make([]string, 0, len(ops))
	for k := range ops {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		v := ops[name]
		switch name {
		case "isNull":
			if v == nil {
				continue
			}
			if b, _ := v.(bool); b {
				out = append(out, col+" IS NULL")
			} else {
				out = append(out, col+" IS NOT NULL")
			}
		case "in":
			items, ok := v.([]any)
			if v == nil {
				continue
			}
			if !ok {
				return nil, userErr("%s.in must be a list", f.Name)
			}
			strs := make([]string, len(items))
			for i, it := range items {
				s, err := literal(f, it)
				if err != nil {
					return nil, err
				}
				strs[i] = s
			}
			out = append(out, col+" = ANY("+q.bind(strs)+"::"+f.PGType+"[])")
		case "startsWith":
			if v == nil {
				continue
			}
			s, err := literal(f, v)
			if err != nil {
				return nil, err
			}
			esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
			out = append(out, col+" LIKE "+q.bind(esc+"%")+` ESCAPE '\'`)
		default:
			var sqlOp string
			for _, o := range opsFor(f.Scalar) {
				if o.name == name {
					sqlOp = o.sql
				}
			}
			if sqlOp == "" {
				return nil, userErr("%s doesn't support %s", f.Name, name)
			}
			if v == nil {
				return nil, userErr("%s.%s: null isn't a value to compare with; use isNull", f.Name, name)
			}
			p, err := q.bindAs(f, v)
			if err != nil {
				return nil, err
			}
			out = append(out, col+" "+sqlOp+" "+p)
		}
	}
	return out, nil
}

// outExpr renders a column as JSON the way the API serializes it.
func outExpr(f Field) string {
	col := quote(f.Column)
	switch f.Scalar {
	case BigInt, Decimal:
		if f.List {
			return "to_jsonb(" + col + "::text[])"
		}
		return "to_jsonb(" + col + "::text)"
	}
	return "to_jsonb(" + col + ")"
}

// ---- ordering and cursors ---------------------------------------------------------------------

type orderKey struct {
	f    Field
	desc bool
}

// order is the requested order plus, for keyset paging, the primary key as a tiebreak.
func (x *executor) order(v any) ([]orderKey, error) {
	var keys []orderKey
	seen := map[string]bool{}
	items, _ := v.([]any)
	if m, ok := v.(map[string]any); ok { // a single RowOrder is coerced to a list by the spec, but be lenient
		items = []any{m}
	}
	for _, it := range items {
		m, _ := it.(map[string]any)
		name, _ := m["field"].(string)
		if name == "" {
			return nil, userErr("no column of this table can be ordered by")
		}
		f, ok := x.e.Model.ByName[name]
		if !ok || !f.Orderable() {
			return nil, userErr("%s can't be ordered by", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		dir, _ := m["direction"].(string)
		keys = append(keys, orderKey{f: f, desc: dir == "DESC"})
	}
	for _, f := range x.e.Model.PK {
		if !seen[f.Name] {
			keys = append(keys, orderKey{f: f})
		}
	}
	return keys, nil
}

// cursor is opaque to clients. Keyset (V) when the table has a usable primary key; an offset (O)
// otherwise, which is slower and can skip or repeat rows under concurrent writes
// (docs/decisions/0003). K fingerprints the ordering, so a cursor from one ordering can't be
// replayed against another.
type cursor struct {
	K string   `json:"k"`
	V []string `json:"v,omitempty"`
	O int      `json:"o,omitempty"`
}

func orderFingerprint(keys []orderKey, filter string) string {
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s:%v,", k.f.Name, k.desc)
	}
	b.WriteString(filter)
	sum := sha256.Sum256([]byte(b.String()))
	return base64.RawURLEncoding.EncodeToString(sum[:9])
}

func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s, want string) (cursor, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil || c.K != want {
		return c, userErr("invalid cursor (cursors are only valid for the same where and orderBy)")
	}
	return c, nil
}

// keyset renders "the rows after these values in this order", as the expanded OR form, which works
// for mixed directions: (a > x) OR (a = x AND b < y) OR ...
func keyset(q *sqlQuery, keys []orderKey, vals []string) (string, error) {
	if len(vals) != len(keys) {
		return "", userErr("invalid cursor")
	}
	var ors []string
	for i, k := range keys {
		var ands []string
		for j := 0; j < i; j++ {
			ands = append(ands, quote(keys[j].f.Column)+" = "+q.bind(vals[j])+"::"+keys[j].f.PGType)
		}
		op := ">"
		if k.desc {
			op = "<"
		}
		ands = append(ands, quote(k.f.Column)+" "+op+" "+q.bind(vals[i])+"::"+k.f.PGType)
		ors = append(ors, "("+strings.Join(ands, " AND ")+")")
	}
	return "(" + strings.Join(ors, " OR ") + ")", nil
}

// ---- resolvers --------------------------------------------------------------------------------

type rowObj map[string]json.RawMessage

func (rowObj) typeName() string { return "Row" }

func (r rowObj) resolve(_ context.Context, _ *executor, c collected) (any, error) {
	return r[c.first().Name], nil
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

// selectedRowFields is the union of Row fields selected under every `nodes` of a rows field.
func (x *executor) selectedRowFields(ss []collected) []Field {
	seen := map[string]bool{}
	var out []Field
	for _, c := range ss {
		for _, f := range x.collect(c.subSelections()) {
			name := f.first().Name
			if fld, ok := x.e.Model.ByName[name]; ok && !seen[name] {
				seen[name] = true
				out = append(out, fld)
			}
		}
	}
	return out
}

func (x *executor) rows(ctx context.Context, c collected) (any, error) {
	first, err := x.first(c)
	if err != nil {
		return nil, userErr("%s", err.Error())
	}
	args := x.args(c)
	keys, err := x.order(args["orderBy"])
	if err != nil {
		return nil, err
	}
	var nodes []collected
	wantPage := false
	for _, sub := range x.collect(c.subSelections()) {
		switch sub.first().Name {
		case "nodes":
			nodes = append(nodes, sub)
		case "pageInfo":
			wantPage = true
		}
	}
	fields := x.selectedRowFields(nodes)

	q := &sqlQuery{}
	filter, err := x.where(q, args["where"])
	if err != nil {
		return nil, err
	}
	filterArgs, _ := json.Marshal(q.args) // the filter's text and values both identify it
	fp := orderFingerprint(keys, filter+string(filterArgs))
	keysetMode := len(x.e.Model.PK) > 0

	conds := []string{filter}
	offset := 0
	if after, _ := args["after"].(string); after != "" {
		cur, err := decodeCursor(after, fp)
		if err != nil {
			return nil, err
		}
		if keysetMode {
			ks, err := keyset(q, keys, cur.V)
			if err != nil {
				return nil, err
			}
			conds = append(conds, ks)
		} else {
			offset = cur.O
		}
	}

	sel := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		sel = append(sel, outExpr(f))
	}
	cursorCols := make([]string, len(keys))
	order := make([]string, 0, len(keys)+1)
	for i, k := range keys {
		cursorCols[i] = quote(k.f.Column) + "::text"
		dir := " ASC"
		if k.desc {
			dir = " DESC"
		}
		order = append(order, quote(k.f.Column)+dir)
	}
	if !keysetMode {
		// No key to break ties with. Views and foreign tables have no ctid either, so order by
		// every comparable column after the requested ones: rows that tie on all of them are
		// indistinguishable to the caller anyway. Offsets can still skip or repeat rows under
		// concurrent writes (docs/decisions/0003).
		for _, f := range x.e.Model.Fields {
			if f.Filterable() {
				order = append(order, quote(f.Column))
			}
		}
	}
	sel = append(sel, "jsonb_build_array("+strings.Join(cursorCols, ", ")+")")

	stmt := "SELECT " + strings.Join(sel, ", ") +
		" FROM " + quote(x.e.Model.Def.Table.Schema, x.e.Model.Def.Table.Name) +
		" WHERE " + strings.Join(conds, " AND ")
	if len(order) > 0 {
		stmt += " ORDER BY " + strings.Join(order, ", ")
	}
	stmt +=
		" LIMIT " + q.bind(first+1)
	if offset > 0 {
		stmt += " OFFSET " + q.bind(offset)
	}

	rows, err := x.tx.Query(ctx, stmt, q.args...)
	if err != nil {
		return nil, castError(err)
	}
	defer rows.Close()
	var out list
	var lastCursor []string
	n := 0
	for rows.Next() {
		n++
		if n > first {
			break // the +1 row only says there is a next page
		}
		vals := make([]any, len(fields)+1)
		raws := make([][]byte, len(fields)+1)
		for i := range raws {
			vals[i] = &raws[i]
		}
		if err := rows.Scan(vals...); err != nil {
			return nil, err
		}
		r := rowObj{}
		for i, f := range fields {
			if raws[i] == nil {
				r[f.Name] = json.RawMessage("null")
			} else {
				r[f.Name] = json.RawMessage(raws[i])
			}
		}
		out = append(out, r)
		lastCursor = nil
		_ = json.Unmarshal(raws[len(fields)], &lastCursor)
	}
	if err := rows.Err(); err != nil {
		return nil, castError(err)
	}
	page := pageInfoObj{hasNext: n > first}
	if len(out) > 0 && wantPage {
		cur := cursor{K: fp}
		if keysetMode {
			cur.V = lastCursor
		} else {
			cur.O = offset + len(out)
		}
		s := encodeCursor(cur)
		page.end = &s
	}
	return connectionObj{nodes: out, page: page}, nil
}

func (x *executor) row(ctx context.Context, c collected) (any, error) {
	if len(x.e.Model.PK) != 1 {
		return nil, userErr("this table has no single-column primary key")
	}
	pk := x.e.Model.PK[0]
	fields := x.selectedRowFields([]collected{c})
	q := &sqlQuery{}
	p, err := q.bindAs(pk, x.args(c)[pk.Name])
	if err != nil {
		return nil, err
	}
	sel := []string{"1"}
	for _, f := range fields {
		sel = append(sel, outExpr(f))
	}
	stmt := "SELECT " + strings.Join(sel, ", ") + " FROM " + quote(x.e.Model.Def.Table.Schema, x.e.Model.Def.Table.Name) +
		" WHERE " + quote(pk.Column) + " = " + p + " LIMIT 1"
	vals := make([]any, len(fields)+1)
	var one int
	vals[0] = &one
	raws := make([][]byte, len(fields))
	for i := range raws {
		vals[i+1] = &raws[i]
	}
	err = x.tx.QueryRow(ctx, stmt, q.args...).Scan(vals...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, castError(err)
	}
	r := rowObj{}
	for i, f := range fields {
		if raws[i] == nil {
			r[f.Name] = json.RawMessage("null")
		} else {
			r[f.Name] = json.RawMessage(raws[i])
		}
	}
	return r, nil
}

// castError turns "this value isn't a valid <type>" into a caller error; anything else stays
// internal. Class 22 is Postgres's data-exception class (invalid_text_representation, out of
// range, invalid datetime format, ...): always caused by a request value, since identifiers and
// types come from the snapshot.
func castError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22") {
		return userErr("a value in the query isn't valid for its column: %s", pgErr.Message)
	}
	return err
}

func isStatementTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}
