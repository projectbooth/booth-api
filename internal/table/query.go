package table

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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Limits shared by both protocols: what one request may make the database do (ADR 0100 item 4;
// correctness limits, not quotas). Defaults are docs/design-v0.md §3's.
type Limits struct {
	DefaultFirst int
	MaxFirst     int
	// MaxFilterDepth bounds and/or/not nesting; MaxPredicates the comparisons in one filter;
	// MaxInList an `in` list.
	MaxFilterDepth   int
	MaxPredicates    int
	MaxInList        int
	StatementTimeout time.Duration
}

// DefaultLimits are the defaults the chart will expose once the endpoints are mounted.
var DefaultLimits = Limits{DefaultFirst: 50, MaxFirst: 500, MaxFilterDepth: 4, MaxPredicates: 50, MaxInList: 100, StatementTimeout: 5 * time.Second}

// Error is an error whose message is safe to return to the caller: it describes the request, never
// SQL or the database.
type Error struct {
	Msg string
	// Timeout marks the statement-timeout error, which REST answers with 503 rather than 400.
	Timeout bool
}

func (e *Error) Error() string { return e.Msg }

func userErr(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

// Errorf makes an *Error, for the protocol packages' own request checks.
func Errorf(format string, args ...any) error { return userErr(format, args...) }

// DB is what a request needs: a read-only transaction. *pgxpool.Pool satisfies it.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// WithTx runs fn in a READ ONLY transaction with a statement timeout: the lease is read-only
// already (ADR 0103), this makes it so even if one weren't. A timeout is reported as an *Error.
//
// A connection through the credential sidecar can end at any lease boundary
// (contracts/credential-sidecar.md, "Connection lifetime"), and a pooled one may be found dead
// only when used. If starting the transaction fails that way, before fn has run anything, it is
// retried once on a fresh connection; a failure inside fn is not retried.
func WithTx(ctx context.Context, db DB, timeout time.Duration, fn func(pgx.Tx) error) error {
	tx, err := begin(ctx, db, timeout)
	if err != nil && isConnectionError(err) && ctx.Err() == nil {
		tx, err = begin(ctx, db, timeout)
	}
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // nothing to keep; read-only
	err = fn(tx)
	if err != nil && (errors.Is(err, context.DeadlineExceeded) || isStatementTimeout(err)) {
		return &Error{Msg: fmt.Sprintf("the query took longer than %s and was cancelled", timeout), Timeout: true}
	}
	return err
}

// isConnectionError: anything but an error the server itself reported. begin only issues
// BEGIN READ ONLY and SET LOCAL, so repeating it after any such failure changes nothing.
func isConnectionError(err error) bool {
	var pgErr *pgconn.PgError
	return !errors.As(err, &pgErr)
}

func begin(ctx context.Context, db DB, timeout time.Duration) (pgx.Tx, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds())); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	return tx, nil
}

// ---- limits -----------------------------------------------------------------------------------

// CheckFilter enforces the filter limits on a filter in the shared shape: a map of "and"/"or"
// (lists of filters), "not" (a filter) and field names (maps of operator name to value).
func CheckFilter(v any, l Limits) error {
	return checkFilter(v, 1, new(int), l)
}

func checkFilter(v any, depth int, predicates *int, l Limits) error {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil
	}
	if depth > l.MaxFilterDepth {
		return userErr("filter is nested more than %d deep", l.MaxFilterDepth)
	}
	for k, sub := range m {
		switch k {
		case "and", "or":
			list, _ := sub.([]any)
			for _, s := range list {
				if err := checkFilter(s, depth+1, predicates, l); err != nil {
					return err
				}
			}
		case "not":
			if err := checkFilter(sub, depth+1, predicates, l); err != nil {
				return err
			}
		default:
			ops, _ := sub.(map[string]any)
			for opName, val := range ops {
				*predicates++
				if list, ok := val.([]any); ok && opName == "in" && len(list) > l.MaxInList {
					return userErr("an in list has %d values; the limit is %d", len(list), l.MaxInList)
				}
			}
		}
	}
	if *predicates > l.MaxPredicates {
		return userErr("filter has more than %d comparisons", l.MaxPredicates)
	}
	return nil
}

// CheckFirst refuses (not clamps) a page size over the limit, so a client knows it would otherwise
// have got fewer rows than it asked for.
func CheckFirst(first int, l Limits) error {
	if first < 0 || first > l.MaxFirst {
		return userErr("first must be between 0 and %d", l.MaxFirst)
	}
	return nil
}

// ---- values -----------------------------------------------------------------------------------

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
	Decimal:  regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`), // length is bounded by the request size
	Date:     regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`),
	DateTime: regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?(Z|[+-][0-9]{2}:[0-9]{2})?$`),
	Time:     regexp.MustCompile(`^[0-9]{2}:[0-9]{2}(:[0-9]{2}(\.[0-9]{1,6})?)?$`),
}

// Strict input formats for built-in scalars, needed for REST where every value arrives as a string
// (GraphQL's own validation already types these).
var builtinFormats = map[Scalar]*regexp.Regexp{
	Int:     regexp.MustCompile(`^-?[0-9]{1,10}$`),
	Float:   regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?$`),
	Boolean: regexp.MustCompile(`^(true|false)$`),
}

// literal renders an input value as the text Postgres will cast. Values arrive as strings (always,
// from REST; for custom scalars, from GraphQL), numbers or booleans; strings must match the
// scalar's format.
func literal(f Field, v any) (string, error) {
	s, isString, err := rawLiteral(f, v)
	if err != nil {
		return "", err
	}
	re := scalarFormats[f.Scalar]
	if re == nil && isString {
		re = builtinFormats[f.Scalar]
	}
	if re != nil && !re.MatchString(s) {
		return "", userErr("%s: %q isn't a valid %s", f.Name, s, f.Scalar)
	}
	return s, nil
}

func rawLiteral(f Field, v any) (string, bool, error) {
	switch t := v.(type) {
	case string:
		return t, true, nil
	case bool:
		return strconv.FormatBool(t), false, nil
	case int:
		return strconv.Itoa(t), false, nil
	case int64:
		return strconv.FormatInt(t, 10), false, nil
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), false, nil
	case json.Number:
		return t.String(), false, nil
	case nil:
		return "", false, userErr("%s: null isn't a value to compare with; use isNull", f.Name)
	}
	return "", false, userErr("%s: unsupported value %v", f.Name, v)
}

// ---- filters ----------------------------------------------------------------------------------

// where compiles a filter in the shared shape (see CheckFilter).
func (m Model) where(q *sqlQuery, v any) (string, error) {
	fm, ok := v.(map[string]any)
	if !ok || len(fm) == 0 {
		return "TRUE", nil
	}
	keys := make([]string, 0, len(fm))
	for k := range fm {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable SQL text for the same filter
	var parts []string
	for _, k := range keys {
		sub := fm[k]
		switch k {
		case "and", "or":
			items, _ := sub.([]any)
			if len(items) == 0 {
				continue
			}
			var ps []string
			for _, it := range items {
				p, err := m.where(q, it)
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
			p, err := m.where(q, sub)
			if err != nil {
				return "", err
			}
			parts = append(parts, "NOT ("+p+")")
		default:
			f, ok := m.ByName[k]
			if !ok || !f.Filterable() {
				return "", userErr("%s can't be filtered on", k)
			}
			ops, _ := sub.(map[string]any)
			p, err := compare(q, f, ops)
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

func compare(q *sqlQuery, f Field, ops map[string]any) ([]string, error) {
	col := quote(f.Column)
	names := make([]string, 0, len(ops))
	for k := range ops {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		v := ops[name]
		if !supports(f.Scalar, name) {
			return nil, userErr("%s doesn't support %s", f.Name, name)
		}
		switch name {
		case "isNull":
			if v == nil {
				continue
			}
			b, isBool := v.(bool)
			if s, ok := v.(string); ok && (s == "true" || s == "false") {
				b, isBool = s == "true", true
			}
			if !isBool {
				return nil, userErr("%s.isNull must be true or false", f.Name)
			}
			if b {
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
			if v == nil {
				return nil, userErr("%s.%s: null isn't a value to compare with; use isNull", f.Name, name)
			}
			p, err := q.bindAs(f, v)
			if err != nil {
				return nil, err
			}
			out = append(out, col+" "+sqlOp(f.Scalar, name)+" "+p)
		}
	}
	return out, nil
}

func supports(s Scalar, op string) bool {
	for _, o := range OpsFor(s) {
		if o.Name == op {
			return true
		}
	}
	return false
}

func sqlOp(s Scalar, op string) string {
	for _, o := range OpsFor(s) {
		if o.Name == op {
			return o.SQL
		}
	}
	return ""
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

// OrderBy is one requested sort key, by API field name.
type OrderBy struct {
	Field string
	Desc  bool
}

type orderKey struct {
	f    Field
	desc bool
}

// order is the requested order plus, for keyset paging, the primary key as a tiebreak.
func (m Model) order(req []OrderBy) ([]orderKey, error) {
	var keys []orderKey
	seen := map[string]bool{}
	for _, o := range req {
		f, ok := m.ByName[o.Field]
		if !ok || !f.Orderable() {
			return nil, userErr("%s can't be ordered by", o.Field)
		}
		if seen[o.Field] {
			continue
		}
		seen[o.Field] = true
		keys = append(keys, orderKey{f: f, desc: o.Desc})
	}
	for _, f := range m.PK {
		if !seen[f.Name] {
			keys = append(keys, orderKey{f: f})
		}
	}
	return keys, nil
}

// cursor is opaque to clients. Keyset (V) when the table has a usable primary key; an offset (O)
// otherwise, which is slower and can skip or repeat rows under concurrent writes
// (docs/decisions/0003). K fingerprints the filter and ordering, so a cursor from one can't be
// replayed against another. REST and GraphQL cursors are interchangeable for the same filter and
// ordering, since both compile to the same SQL.
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
		return c, userErr("invalid cursor (cursors are only valid for the same filter and ordering)")
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

// ---- fetching ---------------------------------------------------------------------------------

// Row is one result row: API field name to its JSON value.
type Row map[string]json.RawMessage

// Page asks for one page of rows.
type Page struct {
	// Where is a filter in the shared shape (see CheckFilter); nil means every row.
	Where   any
	OrderBy []OrderBy
	First   int
	// After is the previous page's EndCursor, or "".
	After string
	// Fields are the API fields to return. Nil returns rows with no fields, which still answers
	// whether there are rows and where the page ends.
	Fields []string
}

// Result is one page.
type Result struct {
	Rows    []Row
	HasNext bool
	// EndCursor is set whenever Rows is non-empty.
	EndCursor *string
}

func (m Model) fields(names []string) ([]Field, error) {
	out := make([]Field, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		f, ok := m.ByName[n]
		if !ok {
			return nil, userErr("no field %q", n)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, f)
		}
	}
	return out, nil
}

// Fetch runs one page as one statement: the requested columns, the filter, keyset (or offset)
// pagination, first+1 rows to learn whether there is a next page.
func Fetch(ctx context.Context, tx pgx.Tx, m Model, p Page, l Limits) (Result, error) {
	if err := CheckFirst(p.First, l); err != nil {
		return Result{}, err
	}
	if err := CheckFilter(p.Where, l); err != nil {
		return Result{}, err
	}
	fields, err := m.fields(p.Fields)
	if err != nil {
		return Result{}, err
	}
	keys, err := m.order(p.OrderBy)
	if err != nil {
		return Result{}, err
	}

	q := &sqlQuery{}
	filter, err := m.where(q, p.Where)
	if err != nil {
		return Result{}, err
	}
	filterArgs, _ := json.Marshal(q.args) // the filter's text and values both identify it
	fp := orderFingerprint(keys, filter+string(filterArgs))
	keysetMode := len(m.PK) > 0

	conds := []string{filter}
	offset := 0
	if p.After != "" {
		cur, err := decodeCursor(p.After, fp)
		if err != nil {
			return Result{}, err
		}
		if keysetMode {
			ks, err := keyset(q, keys, cur.V)
			if err != nil {
				return Result{}, err
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
		for _, f := range m.Fields {
			if f.Filterable() {
				order = append(order, quote(f.Column))
			}
		}
	}
	sel = append(sel, "jsonb_build_array("+strings.Join(cursorCols, ", ")+")")

	stmt := "SELECT " + strings.Join(sel, ", ") +
		" FROM " + quote(m.Def.Table.Schema, m.Def.Table.Name) +
		" WHERE " + strings.Join(conds, " AND ")
	if len(order) > 0 {
		stmt += " ORDER BY " + strings.Join(order, ", ")
	}
	stmt += " LIMIT " + q.bind(p.First+1)
	if offset > 0 {
		stmt += " OFFSET " + q.bind(offset)
	}

	rows, err := tx.Query(ctx, stmt, q.args...)
	if err != nil {
		return Result{}, castError(err)
	}
	defer rows.Close()
	res := Result{Rows: []Row{}}
	var lastCursor []string
	n := 0
	for rows.Next() {
		n++
		if n > p.First {
			break // the +1 row only says there is a next page
		}
		raws, err := scanRaw(rows, len(fields)+1)
		if err != nil {
			return Result{}, err
		}
		res.Rows = append(res.Rows, toRow(fields, raws))
		lastCursor = nil
		_ = json.Unmarshal(raws[len(fields)], &lastCursor)
	}
	if err := rows.Err(); err != nil {
		return Result{}, castError(err)
	}
	res.HasNext = n > p.First
	if len(res.Rows) > 0 {
		cur := cursor{K: fp}
		if keysetMode {
			cur.V = lastCursor
		} else {
			cur.O = offset + len(res.Rows)
		}
		s := encodeCursor(cur)
		res.EndCursor = &s
	}
	return res, nil
}

// Get returns one row by a single-column primary key, or found=false.
func Get(ctx context.Context, tx pgx.Tx, m Model, key any, fieldNames []string) (Row, bool, error) {
	if len(m.PK) != 1 {
		return nil, false, userErr("this table has no single-column primary key")
	}
	fields, err := m.fields(fieldNames)
	if err != nil {
		return nil, false, err
	}
	pk := m.PK[0]
	q := &sqlQuery{}
	p, err := q.bindAs(pk, key)
	if err != nil {
		return nil, false, err
	}
	sel := []string{"1"}
	for _, f := range fields {
		sel = append(sel, outExpr(f))
	}
	stmt := "SELECT " + strings.Join(sel, ", ") + " FROM " + quote(m.Def.Table.Schema, m.Def.Table.Name) +
		" WHERE " + quote(pk.Column) + " = " + p + " LIMIT 1"
	rows, err := tx.Query(ctx, stmt, q.args...)
	if err != nil {
		return nil, false, castError(err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, false, castError(err)
		}
		return nil, false, nil
	}
	raws, err := scanRaw(rows, len(fields)+1)
	if err != nil {
		return nil, false, err
	}
	return toRow(fields, raws[1:]), true, nil
}

func scanRaw(rows pgx.Rows, n int) ([][]byte, error) {
	raws := make([][]byte, n)
	vals := make([]any, n)
	for i := range raws {
		vals[i] = &raws[i]
	}
	return raws, rows.Scan(vals...)
}

func toRow(fields []Field, raws [][]byte) Row {
	r := Row{}
	for i, f := range fields {
		if raws[i] == nil {
			r[f.Name] = json.RawMessage("null")
		} else {
			r[f.Name] = json.RawMessage(raws[i])
		}
	}
	return r
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
