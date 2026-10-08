package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/db/dbtest"
	"github.com/projectbooth/booth-api/internal/gql"
	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/table"
)

const ordersDDL = `
CREATE TABLE orders (
	id      bigint PRIMARY KEY,
	region  text NOT NULL,
	amount  numeric(12,2),
	placed  timestamptz NOT NULL,
	paid    boolean NOT NULL DEFAULT false,
	meta    jsonb,
	"limit" integer
);
INSERT INTO orders VALUES
	(9007199254740993, 'emea', 12.50, '2026-10-01T10:00:00Z', true,  '{"k": 1}', 1),
	(2, 'apac', 3.00,  '2026-10-02T10:00:00Z', false, NULL, 2),
	(3, 'emea', NULL,  '2026-10-03T10:00:00Z', true,  NULL, 3),
	(4, 'amer', 100.01, '2026-10-04T10:00:00Z', false, NULL, NULL),
	(5, 'a%b',  7.77,  '2026-10-05T10:00:00Z', true,  NULL, 5),
	(6, 'apac', 0.01,  '2026-10-06T10:00:00Z', false, NULL, 6),
	(7, 'axb',  1.00,  '2026-10-07T10:00:00Z', true,  NULL, 7);`

func fixture(t *testing.T, ddl, name string, l table.Limits) (*Handler, *pgxpool.Pool) {
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
	m := table.NewModel(apidef.Definition{Slug: name, DatasetName: name, Table: ref, Columns: tbl.Columns, PrimaryKey: tbl.PrimaryKey,
		GeneratedAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)})
	return &Handler{Model: m, Limits: l, DB: pool, OnInternalError: func(err error) { t.Errorf("internal error: %v", err) }}, pool
}

type resp struct {
	code int
	hdr  http.Header
	body string
	data []map[string]any
	one  map[string]any
	page struct {
		HasNext    bool    `json:"hasNext"`
		NextCursor *string `json:"nextCursor"`
	}
	err string
}

func get(t *testing.T, h http.Handler, target string) resp {
	t.Helper()
	return do(t, h, http.MethodGet, target)
}

func do(t *testing.T, h http.Handler, method, target string) resp {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	r := resp{code: rec.Code, hdr: rec.Header(), body: rec.Body.String()}
	var raw struct {
		Data  json.RawMessage `json:"data"`
		Page  json.RawMessage `json:"page"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("%s: body isn't JSON: %v\n%s", target, err, r.body)
	}
	r.err = raw.Error.Message
	if len(raw.Data) > 0 && raw.Data[0] == '[' {
		_ = json.Unmarshal(raw.Data, &r.data)
	} else if len(raw.Data) > 0 {
		_ = json.Unmarshal(raw.Data, &r.one)
	}
	if raw.Page != nil {
		_ = json.Unmarshal(raw.Page, &r.page)
	}
	return r
}

func ids(rows []map[string]any) string {
	var s []string
	for _, r := range rows {
		s = append(s, fmt.Sprint(r["id"]))
	}
	return strings.Join(s, ",")
}

func TestList(t *testing.T) {
	h, _ := fixture(t, ordersDDL, "orders", table.DefaultLimits)
	r := get(t, h, "/rows")
	if r.code != 200 || len(r.data) != 7 || r.page.HasNext || r.page.NextCursor != nil {
		t.Fatalf("list = %d %+v %s", r.code, r.page, r.body)
	}
	if r.hdr.Get("Cache-Control") != "no-store" || r.hdr.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", r.hdr)
	}
	// Every field, in column order; bigint and numeric as exact strings. A column named "limit"
	// is a field like any other (filters are namespaced, so it can't collide with ?limit=).
	if !strings.HasPrefix(r.body, `{"data":[{"id":"2","region":"apac","amount":"3.00","placed":"2026-10-02T10:00:00+00:00","paid":false,"meta":null,"limit":2}`) {
		t.Errorf("first row = %.200s", r.body)
	}
	if last := r.data[6]; last["id"] != "9007199254740993" || fmt.Sprint(last["meta"]) != "map[k:1]" {
		t.Errorf("last row = %v", last)
	}

	r = get(t, h, "/rows?fields=region,id&limit=1")
	if !strings.HasPrefix(r.body, `{"data":[{"region":"apac","id":"2"}],"page":{"hasNext":true,"nextCursor":"`) {
		t.Errorf("fields order: %s", r.body)
	}
}

func TestPaging(t *testing.T) {
	h, pool := fixture(t, ordersDDL, "orders", table.DefaultLimits)
	walk := func(query string) string {
		var pages []string
		next := ""
		for range 10 {
			target := "/rows?limit=3&fields=id" + query
			if next != "" {
				target += "&after=" + next
			}
			r := get(t, h, target)
			if r.code != 200 {
				t.Fatalf("%s: %d %s", target, r.code, r.err)
			}
			pages = append(pages, ids(r.data))
			if r.page.NextCursor == nil {
				if r.page.HasNext {
					t.Fatal("hasNext without a cursor")
				}
				return strings.Join(pages, "|")
			}
			next = *r.page.NextCursor
		}
		t.Fatal("paging didn't end")
		return ""
	}
	if got := walk(""); got != "2,3,4|5,6,7|9007199254740993" {
		t.Errorf("by key: %s", got)
	}
	if got := walk("&order=-region,placed"); got != "9007199254740993,3,7|2,6,4|5" {
		t.Errorf("region desc, placed: %s", got)
	}
	if got := walk("&filter[paid]=true"); got != "3,5,7|9007199254740993" {
		t.Errorf("filtered: %s", got)
	}

	// A REST cursor continues a GraphQL query with the same filter and order, and back.
	r := get(t, h, "/rows?limit=2&fields=id&filter[paid]=true&order=-placed")
	e, err := gql.New(h.Model, gql.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	res := e.Execute(context.Background(), pool, gql.Request{
		Query:     `query($a: String) { rows(first: 2, after: $a, where: {paid: {eq: true}}, orderBy: [{field: placed, direction: DESC}]) { nodes { id } pageInfo { endCursor } } }`,
		Variables: map[string]any{"a": *r.page.NextCursor},
	})
	if len(res.Errors) > 0 || !strings.Contains(string(res.Data), `"nodes":[{"id":"3"},{"id":"9007199254740993"}]`) {
		t.Errorf("REST cursor in GraphQL: %s %v (want the page after 7,5)", res.Data, res.Errors)
	}

	// A cursor only fits the filter and order it came from.
	if r := get(t, h, "/rows?filter[paid]=false&after="+*r.page.NextCursor); r.code != 400 || !strings.Contains(r.err, "invalid cursor") {
		t.Errorf("cursor reused with another filter: %d %s", r.code, r.err)
	}
}

func TestFilters(t *testing.T) {
	h, _ := fixture(t, ordersDDL, "orders", table.DefaultLimits)
	for q, want := range map[string]string{
		"filter[region]=apac":                               "2,6",
		"filter[region][eq]=apac":                           "2,6",
		"filter[region][neq]=apac":                          "3,4,5,7,9007199254740993",
		"filter[region][in]=amer&filter[region][in]=apac":   "2,4,6",
		"filter[amount][gt]=7.77":                           "4,9007199254740993",
		"filter[amount][isNull]=true":                       "3",
		"filter[amount][isNull]=false&filter[amount][lt]=1": "6",
		"filter[region][startsWith]=a%25":                   "5", // %25 is a literal %, not a wildcard
		"filter[placed][gte]=2026-10-06T00:00:00Z":          "6,7",
		"filter[paid]=true&filter[region][neq]=emea":        "5,7",
		"filter[id][in]=2&filter[id][in]=9007199254740993":  "2,9007199254740993",
		"filter[limit][gte]=5":                              "5,6,7",
		"filter[region][in]=a,b&filter[region][in]=emea":    "3,9007199254740993", // commas belong to the value
	} {
		r := get(t, h, "/rows?fields=id&"+q)
		if r.code != 200 {
			t.Errorf("%s: %d %s", q, r.code, r.err)
			continue
		}
		if got := ids(r.data); got != want {
			t.Errorf("%s = %s, want %s", q, got, want)
		}
	}
}

func TestErrors(t *testing.T) {
	h, _ := fixture(t, ordersDDL, "orders", table.DefaultLimits)
	var longIn []string
	for i := range 101 {
		longIn = append(longIn, fmt.Sprintf("filter[id][in]=%d", i))
	}
	for q, want := range map[string]string{
		"limt=5":                                    `unknown parameter "limt"`,
		"region=apac":                               `unknown parameter "region"`,
		"filter[nope]=1":                            `no field "nope"`,
		"filter[paid][gt]=true":                     "doesn't support gt",
		"filter[meta][eq]=x":                        "meta can't be filtered on",
		"filter[amount][gt]=lots":                   `isn't a valid Decimal`,
		"filter[limit][gt]=1.5":                     `isn't a valid Int`,
		"filter[paid]=yes":                          `isn't a valid Boolean`,
		"filter[placed][lt]=yesterday":              `isn't a valid DateTime`,
		"filter[amount][isNull]=maybe":              "must be true or false",
		"filter[region]=a&filter[region][eq]=b":     "given twice",
		"filter[region][eq]=a&filter[region][eq]=b": "must be given once",
		"limit=501":                                 "between 0 and 500",
		"limit=-1":                                  "between 0 and 500",
		"limit=ten":                                 "limit must be one integer",
		"order=nope":                                "nope can't be ordered by",
		"order=amount":                              "amount can't be ordered by", // nullable
		"fields=id,nope":                            `no field "nope"`,
		"fields=":                                   "fields is empty",
		"after=garbage":                             "invalid cursor",
		strings.Join(longIn, "&"):                   "101 values",
	} {
		r := get(t, h, "/rows?"+q)
		if r.code != 400 || !strings.Contains(r.err, want) {
			t.Errorf("%.60s: %d %q, want 400 containing %q", q, r.code, r.err, want)
		}
		if strings.Contains(r.err, "SELECT") || strings.Contains(r.err, "$1") {
			t.Errorf("%.60s: error leaks SQL: %s", q, r.err)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if r := do(t, h, m, "/rows"); r.code != 405 {
			t.Errorf("%s /rows = %d", m, r.code)
		}
	}
	for _, p := range []string{"/", "/rows/2/extra", "/graphql", "/rowsx"} {
		if r := get(t, h, p); r.code != 404 {
			t.Errorf("%s = %d", p, r.code)
		}
	}
}

func TestGetByKey(t *testing.T) {
	h, _ := fixture(t, ordersDDL, "orders", table.DefaultLimits)
	if r := get(t, h, "/rows/9007199254740993?fields=region,id"); r.code != 200 || r.body != "{\"data\":{\"region\":\"emea\",\"id\":\"9007199254740993\"}}\n" {
		t.Errorf("get = %d %s", r.code, r.body)
	}
	if r := get(t, h, "/rows/999"); r.code != 404 {
		t.Errorf("missing = %d", r.code)
	}
	if r := get(t, h, "/rows/x"); r.code != 400 || !strings.Contains(r.err, "isn't a valid BigInt") {
		t.Errorf("bad key = %d %s", r.code, r.err)
	}
	if r := get(t, h, "/rows/2?filter[paid]=true"); r.code != 400 {
		t.Errorf("filter on get = %d", r.code)
	}

	// Text keys may contain an encoded slash; it stays part of the key.
	th, _ := fixture(t, `CREATE TABLE tags (name text PRIMARY KEY, n int); INSERT INTO tags VALUES ('a/b', 1);`, "tags", table.DefaultLimits)
	if r := get(t, th, "/rows/a%2Fb"); r.code != 200 || fmt.Sprint(r.one["n"]) != "1" {
		t.Errorf("encoded slash key = %d %s", r.code, r.body)
	}

	nh, _ := fixture(t, `CREATE TABLE nopk (x int); INSERT INTO nopk VALUES (1);`, "nopk", table.DefaultLimits)
	if r := get(t, nh, "/rows/1"); r.code != 404 || !strings.Contains(r.err, "no single-column primary key") {
		t.Errorf("no key = %d %s", r.code, r.err)
	}
}

func TestTimeout(t *testing.T) {
	l := table.DefaultLimits
	l.StatementTimeout = 200 * time.Millisecond
	h, _ := fixture(t, `CREATE VIEW slow AS SELECT 1 AS n WHERE pg_sleep(2)::text = '';`, "slow", l)
	if r := get(t, h, "/rows"); r.code != 503 || !strings.Contains(r.err, "longer than 200ms") {
		t.Errorf("timeout = %d %s", r.code, r.err)
	}
}

func TestOpenAPI(t *testing.T) {
	h, _ := fixture(t, ordersDDL, "orders", table.DefaultLimits)
	r := get(t, h, "/openapi.json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.body), &doc); err != nil || r.code != 200 {
		t.Fatalf("openapi = %d %v", r.code, err)
	}
	if doc["openapi"] != "3.1.0" || doc["info"].(map[string]any)["version"] != "2026-10-07T12:00:00Z" {
		t.Errorf("header = %v %v", doc["openapi"], doc["info"])
	}
	paths := doc["paths"].(map[string]any)
	for _, p := range []string{"/rows", "/rows/{key}", "/openapi.json"} {
		if paths[p] == nil {
			t.Errorf("path %s missing", p)
		}
	}
	var params []string
	for _, p := range paths["/rows"].(map[string]any)["get"].(map[string]any)["parameters"].([]any) {
		params = append(params, p.(map[string]any)["name"].(string))
	}
	joined := strings.Join(params, " ")
	for _, want := range []string{"limit", "after", "fields", "order", "filter[region][startsWith]", "filter[amount][gt]", "filter[paid][eq]", "filter[limit][in]"} {
		if !strings.Contains(" "+joined+" ", " "+want+" ") {
			t.Errorf("parameter %s missing from %s", want, joined)
		}
	}
	for _, unwanted := range []string{"filter[meta]", "filter[paid][gt]"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("parameter %s offered but unsupported", unwanted)
		}
	}
	row := doc["components"].(map[string]any)["schemas"].(map[string]any)["Row"].(map[string]any)["properties"].(map[string]any)
	if fmt.Sprint(row["id"]) != "map[description:A 64-bit integer, as a string so no precision is lost. format:int64 pattern:^-?[0-9]+$ type:string]" {
		t.Errorf("id schema = %v", row["id"])
	}
	if fmt.Sprint(row["amount"].(map[string]any)["type"]) != "[string null]" {
		t.Errorf("nullable amount = %v", row["amount"])
	}

	nh, _ := fixture(t, `CREATE TABLE nopk (x int);`, "nopk", table.DefaultLimits)
	var nodoc map[string]any
	_ = json.Unmarshal([]byte(get(t, nh, "/openapi.json").body), &nodoc)
	if nodoc["paths"].(map[string]any)["/rows/{key}"] != nil {
		t.Error("/rows/{key} documented for a table without a key")
	}

	// CI lints the document with an OpenAPI linter (.github/workflows/ci.yml).
	if out := os.Getenv("BOOTH_TEST_OPENAPI_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(r.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
