package gql

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/db/dbtest"
	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/table"
)

// fixture creates a table in a fresh schema, snapshots it the way generation does
// (source.Introspect), and returns an engine for it.
func fixture(t *testing.T, ddl, name string, limits Limits) (*Engine, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	ref := apidef.TableRef{Schema: schema, Name: name}
	tbl, err := source.Introspect(ctx, pool, ref)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(table.NewModel(apidef.Definition{Slug: name, DatasetName: name, Table: ref, Columns: tbl.Columns, PrimaryKey: tbl.PrimaryKey}), limits)
	if err != nil {
		t.Fatal(err)
	}
	e.OnInternalError = func(err error) { t.Errorf("internal error: %v", err) }
	return e, pool
}

const ordersDDL = `
CREATE TABLE orders (
	id      bigint PRIMARY KEY,
	region  text NOT NULL,
	amount  numeric(12,2),
	placed  timestamptz NOT NULL,
	paid    boolean NOT NULL DEFAULT false,
	tags    text[],
	meta    jsonb,
	blob    bytea,
	"Total Amount" integer
);
INSERT INTO orders VALUES
	(9007199254740993, 'emea', 12.50, '2026-10-01T10:00:00Z', true,  '{a,b}', '{"k": [1, 2]}', '\x00', 1),
	(2, 'apac', 3.00,  '2026-10-02T10:00:00Z', false, NULL, NULL, NULL, 2),
	(3, 'emea', NULL,  '2026-10-03T10:00:00Z', true,  '{}', '"s"', NULL, 3),
	(4, 'amer', 100.01, '2026-10-04T10:00:00Z', false, NULL, NULL, NULL, NULL),
	(5, 'a%b',  7.77,  '2026-10-05T10:00:00Z', true,  NULL, NULL, NULL, 5),
	(6, 'apac', 0.01,  '2026-10-06T10:00:00Z', false, NULL, NULL, NULL, 6),
	(7, 'axb',  1.00,  '2026-10-07T10:00:00Z', true,  NULL, NULL, NULL, 7);`

func run(t *testing.T, e *Engine, db table.DB, query string, vars map[string]any) (map[string]any, []string) {
	t.Helper()
	r := e.Execute(context.Background(), db, Request{Query: query, Variables: vars})
	var errs []string
	for _, er := range r.Errors {
		errs = append(errs, er.Message)
	}
	var data map[string]any
	if r.Data != nil {
		dec := json.NewDecoder(strings.NewReader(string(r.Data)))
		dec.UseNumber()
		if err := dec.Decode(&data); err != nil {
			t.Fatalf("data isn't JSON: %v: %s", err, r.Data)
		}
	}
	return data, errs
}

func mustRun(t *testing.T, e *Engine, db table.DB, query string, vars map[string]any) map[string]any {
	t.Helper()
	data, errs := run(t, e, db, query, vars)
	if errs != nil {
		t.Fatalf("errors: %v\nquery: %s", errs, query)
	}
	return data
}

func nodes(data map[string]any, key string) []map[string]any {
	conn := data[key].(map[string]any)
	var out []map[string]any
	for _, n := range conn["nodes"].([]any) {
		out = append(out, n.(map[string]any))
	}
	return out
}

func ids(ns []map[string]any) string {
	var s []string
	for _, n := range ns {
		s = append(s, fmt.Sprint(n["id"]))
	}
	return strings.Join(s, ",")
}

func TestSerialization(t *testing.T) {
	e, pool := fixture(t, ordersDDL, "orders", DefaultLimits)
	data := mustRun(t, e, pool, `{ rows(where: {id: {eq: "9007199254740993"}}) { nodes { id region amount placed paid tags meta Total_Amount __typename } } }`, nil)
	got := nodes(data, "rows")
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	n := got[0]
	// bigint past 2^53 and numeric must survive exactly, as strings.
	if n["id"] != "9007199254740993" || n["amount"] != "12.50" {
		t.Errorf("id/amount = %#v/%#v", n["id"], n["amount"])
	}
	if n["placed"] != "2026-10-01T10:00:00+00:00" || n["paid"] != true || n["__typename"] != "Row" {
		t.Errorf("placed/paid/typename = %v/%v/%v", n["placed"], n["paid"], n["__typename"])
	}
	if fmt.Sprint(n["tags"]) != "[a b]" || fmt.Sprint(n["meta"]) != "map[k:[1 2]]" || fmt.Sprint(n["Total_Amount"]) != "1" {
		t.Errorf("tags/meta/total = %v/%v/%v", n["tags"], n["meta"], n["Total_Amount"])
	}
	// The response keeps the query's field order.
	r := e.Execute(context.Background(), pool, Request{Query: `{ rows(first: 1) { nodes { region id } } }`})
	if !strings.Contains(string(r.Data), `{"region":"apac","id":"2"}`) {
		t.Errorf("field order not preserved: %s", r.Data)
	}
	// bytea is omitted from the schema entirely, so asking for it is a validation error.
	if _, errs := run(t, e, pool, `{ rows { nodes { blob } } }`, nil); len(errs) != 1 || !strings.Contains(errs[0], `"blob"`) {
		t.Errorf("blob: %v", errs)
	}
	// Nulls come back as null.
	data = mustRun(t, e, pool, `{ row(id: "3") { amount tags meta } }`, nil)
	if row := data["row"].(map[string]any); row["amount"] != nil || fmt.Sprint(row["tags"]) != "[]" || row["meta"] != "s" {
		t.Errorf("row 3 = %v", row)
	}
}

func TestPagination(t *testing.T) {
	e, pool := fixture(t, ordersDDL, "orders", DefaultLimits)
	walk := func(orderBy string) string {
		var all []string
		after := ""
		for page := 0; page < 10; page++ {
			data := mustRun(t, e, pool, `query($a: String) { rows(first: 3, after: $a, orderBy: `+orderBy+`) { nodes { id } pageInfo { hasNextPage endCursor } } }`, map[string]any{"a": after})
			ns := nodes(data, "rows")
			if ids(ns) != "" {
				all = append(all, ids(ns))
			}
			pi := data["rows"].(map[string]any)["pageInfo"].(map[string]any)
			if pi["hasNextPage"] != true {
				return strings.Join(all, "|")
			}
			after = pi["endCursor"].(string)
		}
		t.Fatal("pagination didn't terminate")
		return ""
	}
	if got := walk(`[]`); got != "2,3,4|5,6,7|9007199254740993" {
		t.Errorf("by primary key: %s", got)
	}
	// Mixed directions with ties (two apac, two emea): the key breaks ties, nothing repeats or drops.
	if got := walk(`[{field: region, direction: DESC}, {field: placed}]`); got != "9007199254740993,3,7|2,6,4|5" {
		t.Errorf("region desc, placed asc: %s", got)
	}
	if got := walk(`[{field: paid}]`); got != "2,4,6|3,5,7|9007199254740993" {
		t.Errorf("by boolean: %s", got)
	}

	// A cursor is bound to its ordering and filter.
	data := mustRun(t, e, pool, `{ rows(first: 1) { pageInfo { endCursor } } }`, nil)
	cur := data["rows"].(map[string]any)["pageInfo"].(map[string]any)["endCursor"].(string)
	for _, q := range []string{
		`query($a: String) { rows(after: $a, orderBy: [{field: region}]) { nodes { id } } }`,
		`query($a: String) { rows(after: $a, where: {paid: {eq: true}}) { nodes { id } } }`,
		`{ rows(after: "not-a-cursor") { nodes { id } } }`,
	} {
		if _, errs := run(t, e, pool, q, map[string]any{"a": cur}); len(errs) != 1 || !strings.Contains(errs[0], "invalid cursor") {
			t.Errorf("%s: %v", q, errs)
		}
	}

	// first: 0 returns no rows but still says whether there are any.
	data = mustRun(t, e, pool, `{ rows(first: 0) { nodes { id } pageInfo { hasNextPage endCursor } } }`, nil)
	if pi := data["rows"].(map[string]any)["pageInfo"].(map[string]any); pi["hasNextPage"] != true || pi["endCursor"] != nil {
		t.Errorf("first: 0 pageInfo = %v", pi)
	}
}

func TestPaginationWithoutPrimaryKey(t *testing.T) {
	e, pool := fixture(t, `CREATE TABLE events (name text NOT NULL, n int); INSERT INTO events SELECT 'e' || i, i FROM generate_series(1, 5) i;`, "events", DefaultLimits)
	if strings.Contains(e.SDL, "row(") {
		t.Error("row(pk) offered without a primary key")
	}
	var seen []string
	after := ""
	for page := 0; page < 5; page++ {
		data := mustRun(t, e, pool, `query($a: String) { rows(first: 2, after: $a, orderBy: [{field: name}]) { nodes { name } pageInfo { hasNextPage endCursor } } }`, map[string]any{"a": after})
		for _, n := range nodes(data, "rows") {
			seen = append(seen, n["name"].(string))
		}
		pi := data["rows"].(map[string]any)["pageInfo"].(map[string]any)
		if pi["hasNextPage"] != true {
			break
		}
		after = pi["endCursor"].(string)
	}
	if got := strings.Join(seen, ","); got != "e1,e2,e3,e4,e5" {
		t.Errorf("offset paging: %s", got)
	}
}

// A view has no key and no ctid; offset paging must still work over it.
func TestPaginationOverAView(t *testing.T) {
	e, pool := fixture(t, `CREATE TABLE t (n int); INSERT INTO t SELECT generate_series(1, 5); CREATE VIEW v AS SELECT n, n % 2 = 0 AS even FROM t;`, "v", DefaultLimits)
	data := mustRun(t, e, pool, `{ rows(first: 3) { nodes { n } pageInfo { endCursor } } }`, nil)
	cur := data["rows"].(map[string]any)["pageInfo"].(map[string]any)["endCursor"]
	data = mustRun(t, e, pool, `query($a: String) { rows(first: 3, after: $a) { nodes { n } pageInfo { hasNextPage } } }`, map[string]any{"a": cur})
	if got := fmt.Sprint(nodes(data, "rows")); got != "[map[n:4] map[n:5]]" {
		t.Errorf("second page over a view = %s", got)
	}
}

func TestFilters(t *testing.T) {
	e, pool := fixture(t, ordersDDL, "orders", DefaultLimits)
	for filter, want := range map[string]string{
		`{region: {eq: "apac"}}`:                                     "2,6",
		`{region: {neq: "apac"}}`:                                    "3,4,5,7,9007199254740993",
		`{region: {in: ["amer", "apac"]}}`:                           "2,4,6",
		`{amount: {gt: "7.77"}}`:                                     "4,9007199254740993",
		`{amount: {gte: 7.77}}`:                                      "4,5,9007199254740993",
		`{amount: {isNull: true}}`:                                   "3",
		`{amount: {isNull: false, lt: "1"}}`:                         "6",
		`{region: {startsWith: "a%"}}`:                               "5", // % is literal, not a wildcard
		`{region: {startsWith: "a"}}`:                                "2,4,5,6,7",
		`{placed: {gte: "2026-10-06T00:00:00Z"}}`:                    "6,7",
		`{or: [{region: {eq: "amer"}}, {paid: {eq: false}}]}`:        "2,4,6",
		`{and: [{paid: {eq: true}}, {not: {region: {eq: "emea"}}}]}`: "5,7",
		`{id: {in: ["2", "9007199254740993"]}}`:                      "2,9007199254740993",
		`{}`:                                                         "2,3,4,5,6,7,9007199254740993",
	} {
		data, errs := run(t, e, pool, `{ rows(where: `+filter+`) { nodes { id } } }`, nil)
		if errs != nil {
			t.Errorf("%s: %v", filter, errs)
			continue
		}
		if got := ids(nodes(data, "rows")); got != want {
			t.Errorf("%s = %s, want %s", filter, got, want)
		}
	}
	for filter, wantErr := range map[string]string{
		`{amount: {gt: "lots"}}`:            "isn't a valid Decimal",
		`{placed: {lt: "yesterday"}}`:       "isn't a valid DateTime", // Postgres would accept it
		`{amount: {lt: "1e3"}}`:             "isn't a valid Decimal",
		`{id: {eq: "12.5"}}`:                "isn't a valid BigInt",
		`{id: {eq: "9999999999999999999"}}`: "isn't valid for its column", // in format, out of range: Postgres says so
		`{region: {eq: null}}`:              "use isNull",
		`{tags: {eq: "a"}}`:                 `"tags"`, // arrays aren't filterable: not in RowFilter
		`{meta: {eq: "a"}}`:                 `"meta"`, // nor JSON
		`{paid: {gt: true}}`:                `"gt"`,   // booleans only compare for equality
		`{region: {like: "a"}}`:             `"like"`, // no unanchored match
	} {
		_, errs := run(t, e, pool, `{ rows(where: `+filter+`) { nodes { id } } }`, nil)
		if len(errs) != 1 || !strings.Contains(errs[0], wantErr) {
			t.Errorf("%s: errors %v, want one containing %q", filter, errs, wantErr)
		}
		for _, msg := range errs {
			if strings.Contains(msg, "SELECT") || strings.Contains(msg, "$1") {
				t.Errorf("%s: an error leaks SQL: %s", filter, msg)
			}
		}
	}
}

func TestRowByKey(t *testing.T) {
	e, pool := fixture(t, ordersDDL, "orders", DefaultLimits)
	data := mustRun(t, e, pool, `{ a: row(id: "2") { region } b: row(id: "999") { region } }`, nil)
	if fmt.Sprint(data["a"]) != "map[region:apac]" || data["b"] != nil {
		t.Errorf("row = %v", data)
	}
	if _, errs := run(t, e, pool, `{ row(id: "x") { region } }`, nil); len(errs) != 1 || !strings.Contains(errs[0], "isn't a valid BigInt") {
		t.Errorf("bad key: %v", errs)
	}
}

func TestAliasesFragmentsDirectives(t *testing.T) {
	e, pool := fixture(t, ordersDDL, "orders", DefaultLimits)
	q := `query Q($withRegion: Boolean!, $skipPaid: Boolean!) {
		cheap: rows(where: {amount: {lt: "1.5"}}) { nodes { ...Ids  region @include(if: $withRegion) } }
		paid: rows(where: {paid: {eq: true}}, first: 2) { nodes { id paid @skip(if: $skipPaid) ... on Row { amount } } pageInfo { hasNextPage } }
		__typename
	}
	fragment Ids on Row { id }`
	data := mustRun(t, e, pool, q, map[string]any{"withRegion": true, "skipPaid": true})
	if got := fmt.Sprint(nodes(data, "cheap")); got != "[map[id:6 region:apac] map[id:7 region:axb]]" {
		t.Errorf("cheap = %s", got)
	}
	if got := fmt.Sprint(nodes(data, "paid")); got != "[map[amount:<nil> id:3] map[amount:7.77 id:5]]" {
		t.Errorf("paid = %s", got)
	}
	if data["__typename"] != "Query" {
		t.Errorf("__typename = %v", data["__typename"])
	}
	// Same response key twice merges selections.
	data = mustRun(t, e, pool, `{ rows(first: 1) { nodes { id } } rows(first: 1) { nodes { region } } }`, nil)
	if got := fmt.Sprint(nodes(data, "rows")); got != "[map[id:2 region:apac]]" {
		t.Errorf("merged = %s", got)
	}
}

func TestLimits(t *testing.T) {
	e, pool := fixture(t, ordersDDL, "orders", DefaultLimits)
	many := func(n int, field string) string {
		var b strings.Builder
		b.WriteString("{")
		for i := range n {
			fmt.Fprintf(&b, " r%d: %s", i, field)
		}
		return b.String() + " }"
	}
	deepFilter := `{rows(where: {and: [{and: [{and: [{and: [{and: [{id: {eq: "1"}}]}]}]}]}]}) { nodes { id } }}`
	var bigIn []string
	for i := range 101 {
		bigIn = append(bigIn, fmt.Sprintf("%q", fmt.Sprint(i)))
	}
	for name, tc := range map[string]struct{ q, want string }{
		"first over the cap":   {`{ rows(first: 501) { nodes { id } } }`, "between 0 and 500"},
		"negative first":       {`{ rows(first: -1) { nodes { id } } }`, "between 0 and 500"},
		"too many root fields": {many(6, "row(id: \"2\") { id }"), "6 root fields"},
		"too expensive":        {many(3, "rows(first: 500) { nodes { id region amount placed paid tags meta } }"), "over the limit of 10000"},
		"filter too deep":      {deepFilter, "nested more than 4"},
		"in list too long":     {`{ rows(where: {id: {in: [` + strings.Join(bigIn, ",") + `]}}) { nodes { id } } }`, "101 values"},
		"query too large":      {`{ rows { nodes { id } } }` + strings.Repeat(" ", 16<<10), "larger than"},
		"mutation":             {`mutation { rows { nodes { id } } }`, "Schema does not support operation type \"mutation\""},
	} {
		_, errs := run(t, e, pool, tc.q, nil)
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "; "), tc.want) {
			t.Errorf("%s: errors %v, want %q", name, errs, tc.want)
		}
	}
	// Within the limits: 2 × 500 × (7 + 1) = 8000.
	if _, errs := run(t, e, pool, many(2, "rows(first: 500) { nodes { id region amount placed paid tags meta } }"), nil); errs != nil {
		t.Errorf("a query under every limit was refused: %v", errs)
	}

	tight := *e
	tight.Limits.MaxDepth = 2
	if _, errs := run(t, &tight, pool, `{ rows { nodes { id } } }`, nil); len(errs) != 1 || !strings.Contains(errs[0], "nested 3 deep") {
		t.Errorf("depth: %v", errs)
	}
}

// The full introspection query graphql-js and GraphiQL send must work, and fit the separate
// introspection depth limit while the data depth limit stays small.
func TestIntrospection(t *testing.T) {
	e, pool := fixture(t, ordersDDL, "orders", DefaultLimits)
	data := mustRun(t, e, pool, standardIntrospectionQuery, nil)
	schema := data["__schema"].(map[string]any)
	if schema["queryType"].(map[string]any)["name"] != "Query" {
		t.Errorf("queryType = %v", schema["queryType"])
	}
	types := map[string]map[string]any{}
	for _, ty := range schema["types"].([]any) {
		m := ty.(map[string]any)
		types[m["name"].(string)] = m
	}
	var rowFields []string
	for _, f := range types["Row"]["fields"].([]any) {
		rowFields = append(rowFields, f.(map[string]any)["name"].(string))
	}
	if got := strings.Join(rowFields, ","); got != "id,region,amount,placed,paid,tags,meta,Total_Amount" {
		t.Errorf("Row fields = %s", got)
	}
	for _, name := range []string{"BigInt", "Decimal", "DateTime", "JSON", "RowFilter", "StringFilter", "RowField", "OrderDirection", "__Schema"} {
		if types[name] == nil {
			t.Errorf("type %s missing", name)
		}
	}
	if types["Mutation"] != nil {
		t.Error("a Mutation type exists")
	}
	// rows' `first` default and the id field's NON_NULL wrapping come through.
	q := mustRun(t, e, pool, `{ __type(name: "Query") { fields { name args { name defaultValue type { kind name ofType { name } } } } } }`, nil)
	b, _ := json.Marshal(q)
	for _, want := range []string{`"defaultValue":"50"`, `{"kind":"SCALAR","name":"Int","ofType":null}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("__type(Query) lacks %s: %s", want, b)
		}
	}
	if d := mustRun(t, e, pool, `{ __type(name: "Nope") { name } }`, nil); d["__type"] != nil {
		t.Errorf("unknown type = %v", d["__type"])
	}
}

func TestStatementTimeout(t *testing.T) {
	l := DefaultLimits
	l.StatementTimeout = 200 * time.Millisecond
	e, pool := fixture(t, `CREATE VIEW slow AS SELECT 1 AS n WHERE pg_sleep(2)::text = '';`, "slow", l)
	start := time.Now()
	_, errs := run(t, e, pool, `{ rows { nodes { n } } }`, nil)
	if len(errs) != 1 || !strings.Contains(errs[0], "longer than 200ms") {
		t.Errorf("errors = %v", errs)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Errorf("took %v: the statement timeout didn't apply", time.Since(start))
	}
}

func TestReadOnly(t *testing.T) {
	e, pool := fixture(t, ordersDDL, "orders", DefaultLimits)
	// Even with a writable role, the transaction is read-only.
	err := table.WithTx(context.Background(), pool, e.Limits.StatementTimeout, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `DELETE FROM orders`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("a write inside the API's transaction: %v", err)
	}
	if !reflect.DeepEqual(ids(nodes(mustRun(t, e, pool, `{ rows(first: 1) { nodes { id } } }`, nil), "rows")), "2") {
		t.Error("rows were deleted")
	}
}

// standardIntrospectionQuery is graphql-js's getIntrospectionQuery() output (descriptions on,
// no specifiedByUrl/directiveIsRepeatable/oneOf), as GraphiQL sends it.
const standardIntrospectionQuery = `
query IntrospectionQuery {
  __schema {
    queryType { name }
    mutationType { name }
    subscriptionType { name }
    types { ...FullType }
    directives { name description locations args { ...InputValue } }
  }
}
fragment FullType on __Type {
  kind name description
  fields(includeDeprecated: true) { name description args { ...InputValue } type { ...TypeRef } isDeprecated deprecationReason }
  inputFields { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) { name description isDeprecated deprecationReason }
  possibleTypes { ...TypeRef }
}
fragment InputValue on __InputValue { name description type { ...TypeRef } defaultValue }
fragment TypeRef on __Type {
  kind name
  ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } } }
}`
