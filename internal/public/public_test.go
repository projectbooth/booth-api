package public_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/db/dbtest"
	"github.com/projectbooth/booth-api/internal/gql"
	"github.com/projectbooth/booth-api/internal/keys"
	"github.com/projectbooth/booth-api/internal/public"
	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/store"
)

// pools stands in for the per-workspace sidecar pools (ADR 0103): it records who asked and can be
// told to refuse the way the real one does.
type pools struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	err  error
	asks []string // "workspace/owner"
}

func (p *pools) Pool(_ context.Context, ws, owner string) (*pgxpool.Pool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asks = append(p.asks, ws+"/"+owner)
	if p.err != nil {
		return nil, p.err
	}
	return p.pool, nil
}

type env struct {
	t     *testing.T
	h     http.Handler
	st    *store.Store
	keys  keys.Service
	pools *pools
	pool  *pgxpool.Pool
	defs  map[string]apidef.Definition // by slug
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE orders (id bigint PRIMARY KEY, region text NOT NULL, amount numeric(12,2));
		INSERT INTO orders VALUES (1, 'emea', 12.50), (2, 'apac', 3.00);
		CREATE TABLE customers (id int PRIMARY KEY, name text);
		INSERT INTO customers VALUES (1, 'Ada');`); err != nil {
		t.Fatal(err)
	}
	st := store.New(pool)
	e := &env{t: t, st: st, keys: keys.Service{Store: st}, pools: &pools{pool: pool}, pool: pool, defs: map[string]apidef.Definition{}}
	// Three APIs: two in acme, one in globex over the same table.
	for _, a := range []struct{ ws, ds, name, table string }{
		{"acme", "ds-orders", "Orders", "orders"},
		{"acme", "ds-customers", "Customers", "customers"},
		{"globex", "ds-g-orders", "Globex orders", "orders"},
	} {
		tbl, err := source.Introspect(ctx, pool, apidef.TableRef{Schema: schema, Name: a.table})
		if err != nil {
			t.Fatal(err)
		}
		d, err := st.CreateAPI(ctx, apidef.Definition{Workspace: a.ws, DatasetID: a.ds, DatasetName: a.name,
			Table: apidef.TableRef{Schema: schema, Name: a.table}, Columns: tbl.Columns, PrimaryKey: tbl.PrimaryKey, CreatedBy: "creator"})
		if err != nil {
			t.Fatal(err)
		}
		e.defs[d.Slug] = d
	}
	e.h = public.NewHandler(public.Deps{Keys: e.keys, APIs: st, Pools: e.pools, Limits: gql.DefaultLimits})
	return e
}

func (e *env) key(ws, owner string, datasets ...string) string {
	e.t.Helper()
	k, err := e.keys.Issue(context.Background(), ws, "k", datasets, owner, owner)
	if err != nil {
		e.t.Fatal(err)
	}
	return k.Secret
}

type result struct {
	code int
	hdr  http.Header
	body string
	json map[string]any
}

func (e *env) do(method, path, key string, body string, hdr ...string) result {
	e.t.Helper()
	var rd *bytes.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	r := result{code: rec.Code, hdr: rec.Header(), body: rec.Body.String()}
	_ = json.Unmarshal(rec.Body.Bytes(), &r.json)
	return r
}

func msg(r result) string {
	if e, ok := r.json["error"].(map[string]any); ok {
		return e["message"].(string)
	}
	if errs, ok := r.json["errors"].([]any); ok && len(errs) > 0 {
		return errs[0].(map[string]any)["message"].(string)
	}
	return ""
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	good := e.key("acme", "alice", "ds-orders")
	revoked := e.key("acme", "alice", "ds-orders")
	var revokedID string
	ks, _ := e.keys.List(context.Background(), "acme")
	for _, k := range ks {
		if strings.Contains(revoked, k.ID) {
			revokedID = k.ID
		}
	}
	if _, err := e.keys.Revoke(context.Background(), "acme", revokedID, "alice"); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		auth string
		code int
	}{
		"no header":        {"", 401},
		"not bearer":       {"Basic " + good, 401},
		"wrong secret":     {"Bearer " + good[:len(good)-3] + "AAA", 401},
		"revoked":          {"Bearer " + revoked, 401},
		"a platform JWT":   {"Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.sig", 401},
		"bearer, no key":   {"Bearer ", 401},
		"valid":            {"Bearer " + good, 200},
		"lowercase bearer": {"bearer " + good, 200},
	} {
		req := httptest.NewRequest("GET", "/v1/orders/rows", nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != tc.code {
			t.Errorf("%s: %d, want %d: %s", name, rec.Code, tc.code, rec.Body)
		}
		if rec.Code == 401 && !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: 401 without a Bearer challenge", name)
		}
	}
	// A 401 on the GraphQL path is GraphQL-shaped.
	if r := e.do("POST", "/v1/orders/graphql", "", `{"query":"{ rows { nodes { id } } }"}`); r.code != 401 || r.json["errors"] == nil {
		t.Errorf("graphql 401 = %d %s", r.code, r.body)
	}
}

func TestScopeAndWorkspace(t *testing.T) {
	e := newEnv(t)
	k := e.key("acme", "alice", "ds-orders")

	if r := e.do("GET", "/v1/orders/rows", k, ""); r.code != 200 || len(r.json["data"].([]any)) != 2 {
		t.Fatalf("in scope: %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/v1/customers/rows", k, ""); r.code != 403 || !strings.Contains(msg(r), "isn't scoped") {
		t.Errorf("out of scope: %d %s", r.code, r.body)
	}
	// globex's API over the same table is invisible to an acme key: its slug doesn't exist in acme.
	if r := e.do("GET", "/v1/globex-orders/rows", k, ""); r.code != 404 {
		t.Errorf("another workspace's API: %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/v1/nope/rows", k, ""); r.code != 404 {
		t.Errorf("unknown API: %d", r.code)
	}
	// Forged trust headers change nothing: the workspace and role come from the key alone.
	r := e.do("GET", "/v1/globex-orders/rows", k, "", "X-Booth-Workspace", "globex", "X-Booth-Role", "owner",
		"X-Booth-Identity", "forged.jwt.value")
	if r.code != 404 {
		t.Errorf("X-Booth-* headers moved the key into globex: %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/v1/customers/rows", k, "", "X-Booth-Role", "owner"); r.code != 403 {
		t.Errorf("X-Booth-Role widened the key's scope: %d", r.code)
	}

	// The database is asked for as the key's creator, in the key's workspace (ADR 0103).
	e.pools.mu.Lock()
	asks := strings.Join(e.pools.asks, ",")
	e.pools.mu.Unlock()
	if asks != "acme/alice" {
		t.Errorf("pool requests = %s, want only acme/alice", asks)
	}
}

func TestGraphQLAndOpenAPI(t *testing.T) {
	e := newEnv(t)
	k := e.key("acme", "alice", "ds-orders")
	r := e.do("POST", "/v1/orders/graphql", k, `{"query":"query($r: String) { rows(where: {region: {eq: $r}}) { nodes { id amount } } }","variables":{"r":"emea"}}`)
	if r.code != 200 || !strings.Contains(r.body, `"nodes":[{"id":"1","amount":"12.50"}]`) {
		t.Errorf("graphql POST: %d %s", r.code, r.body)
	}
	q := url.Values{"query": {"{ rows { nodes { id } } }"}}
	if r := e.do("GET", "/v1/orders/graphql?"+q.Encode(), k, ""); r.code != 200 || !strings.Contains(r.body, `"id":"2"`) {
		t.Errorf("graphql GET: %d %s", r.code, r.body)
	}
	if r := e.do("POST", "/v1/orders/graphql", k, `{"query":"mutation { x }"}`); r.code != 200 || !strings.Contains(r.body, "mutation") {
		t.Errorf("mutation: %d %s", r.code, r.body)
	}
	if r := e.do("PUT", "/v1/orders/graphql", k, `{}`); r.code != 405 {
		t.Errorf("PUT graphql: %d", r.code)
	}
	if r := e.do("POST", "/v1/orders/graphql", k, "not json"); r.code != 400 {
		t.Errorf("bad body: %d", r.code)
	}
	if r := e.do("POST", "/v1/orders/graphql", k, `{"query":"`+strings.Repeat("x", 70<<10)+`"}`); r.code != 413 {
		t.Errorf("oversized body: %d", r.code)
	}
	if r := e.do("POST", "/v1/orders/rows", k, ""); r.code != 405 {
		t.Errorf("POST rows: %d", r.code)
	}
	if r := e.do("GET", "/v1/orders/rows/2", k, ""); r.code != 200 || !strings.Contains(r.body, `"region":"apac"`) {
		t.Errorf("get by key: %d %s", r.code, r.body)
	}

	// The schema documents need a key in scope but not the database.
	e.pools.mu.Lock()
	e.pools.err = source.ErrUnavailable
	e.pools.mu.Unlock()
	if r := e.do("GET", "/v1/orders/openapi.json", k, ""); r.code != 200 || r.json["openapi"] != "3.1.0" {
		t.Errorf("openapi with no data source: %d %.100s", r.code, r.body)
	}
	if r := e.do("GET", "/v1/customers/openapi.json", k, ""); r.code != 403 {
		t.Errorf("openapi out of scope: %d", r.code)
	}
}

func TestDataSourceRefusals(t *testing.T) {
	e := newEnv(t)
	k := e.key("acme", "alice", "ds-orders")
	e.pools.err = source.ErrOwnerNoAccess
	if r := e.do("GET", "/v1/orders/rows", k, ""); r.code != 403 || !strings.Contains(msg(r), "creator no longer has access") {
		t.Errorf("owner without access: %d %s", r.code, r.body)
	}
	if r := e.do("POST", "/v1/orders/graphql", k, `{"query":"{ rows { nodes { id } } }"}`); r.code != 403 || r.json["errors"] == nil {
		t.Errorf("graphql owner without access: %d %s", r.code, r.body)
	}
	e.pools.err = source.ErrUnavailable
	if r := e.do("GET", "/v1/orders/rows", k, ""); r.code != 503 {
		t.Errorf("no data source: %d", r.code)
	}
	e.pools.err = errors.New("boom")
	if r := e.do("GET", "/v1/orders/rows", k, ""); r.code != 500 || strings.Contains(r.body, "boom") {
		t.Errorf("internal: %d %s", r.code, r.body)
	}
}

// Regenerating an API is picked up on the next request (the engine cache is keyed by generatedAt).
func TestRegenerationIsPickedUp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k := e.key("acme", "alice", "ds-orders")
	if r := e.do("GET", "/v1/orders/rows?fields=id", k, ""); r.code != 200 {
		t.Fatal(r.body)
	}
	if _, err := e.pool.Exec(ctx, `ALTER TABLE orders ADD COLUMN note text DEFAULT 'n'`); err != nil {
		t.Fatal(err)
	}
	d := e.defs["orders"]
	d.Columns = append(d.Columns, apidef.Column{Name: "note", Type: "text", Nullable: true})
	time.Sleep(10 * time.Millisecond) // generated_at is now(); make sure it moves
	if _, err := e.st.UpdateSnapshot(ctx, d); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/v1/orders/rows?fields=note&limit=1", k, ""); r.code != 200 || !strings.Contains(r.body, `"note":"n"`) {
		t.Errorf("after regenerate: %d %s", r.code, r.body)
	}
}

func TestLastUsed(t *testing.T) {
	e := newEnv(t)
	k := e.key("acme", "alice", "ds-orders")
	e.do("GET", "/v1/orders/rows", k, "")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ks, _ := e.keys.List(context.Background(), "acme")
		if len(ks) == 1 && ks[0].LastUsedAt != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("last_used_at never recorded")
}

func TestPaths(t *testing.T) {
	e := newEnv(t)
	k := e.key("acme", "alice", "ds-orders")
	for _, p := range []string{"/v1/", "/v1/orders", "/v1/orders/", "/v1/Orders/rows", "/v1/../api/keys", "/v1/orders/graphqlx"} {
		if r := e.do("GET", p, k, ""); r.code != 404 {
			t.Errorf("%s = %d", p, r.code)
		}
	}
}
