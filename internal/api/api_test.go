package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-api/internal/api"
	"github.com/projectbooth/booth-api/internal/apis"
	"github.com/projectbooth/booth-api/internal/auth"
	"github.com/projectbooth/booth-api/internal/auth/authtest"
	"github.com/projectbooth/booth-api/internal/catalog"
	"github.com/projectbooth/booth-api/internal/db/dbtest"
	"github.com/projectbooth/booth-api/internal/keys"
	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/store"
)

// fixturePools stands in for the per-workspace sidecar pools (ADR 0103), which aren't built: every
// workspace reads the test schema, where the fixture tables live.
type fixturePools struct{ pool *pgxpool.Pool }

func (f fixturePools) Pool(context.Context, string, string) (*pgxpool.Pool, error) {
	return f.pool, nil
}

type env struct {
	t       *testing.T
	h       http.Handler
	idp     *authtest.IdP
	pool    *pgxpool.Pool
	keys    keys.Service
	schema  string
	catalog map[string]string // dataset id -> JSON the fake catalog serves
}

func newEnv(t *testing.T, pools source.Pools) *env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	st := store.New(pool)
	e := &env{t: t, idp: authtest.New(t), pool: pool, catalog: map[string]string{}}
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&e.schema); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE orders (id bigint PRIMARY KEY, amount numeric(12,2), placed timestamptz NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	// The fake catalog behind a fake gateway: it serves a dataset only to a request carrying a
	// bearer token and the X-Workspace the dataset belongs to ("acme").
	cat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.Header.Get("X-Workspace") != "acme" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, ok := e.catalog[strings.TrimPrefix(r.URL.Path, "/modules/catalog/api/datasets/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(cat.Close)

	if pools == nil {
		pools = fixturePools{pool}
	}
	v, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: e.idp.URL, ClientID: "booth-api"})
	if err != nil {
		t.Fatal(err)
	}
	e.keys = keys.Service{Store: st}
	e.h = api.NewHandler(api.Deps{
		Verifier: func() auth.TokenVerifier { return v },
		APIs:     apis.Service{Catalog: &catalog.Client{CoreURL: cat.URL}, Store: st, Pools: pools},
		Keys:     e.keys,
	})
	return e
}

func (e *env) dataset(id, name, table, schemaJSON string) {
	e.catalog[id] = fmt.Sprintf(`{"id":%q,"name":%q,"format":"postgres",%q:{"schema":%q,"name":%q},"schema":%s}`,
		id, name, catalog.PostgresTableField, e.schema, table, schemaJSON)
}

func (e *env) token(sub, groups string) string {
	return e.idp.Mint(e.t, authtest.Token{Subject: sub, Groups: strings.Split(groups, ","), Extra: map[string]any{"preferred_username": sub + "-name"}})
}

func (e *env) do(token, workspace, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	e.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(auth.HeaderBoothWorkspace, workspace)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (e *env) want(rec *httptest.ResponseRecorder, status int, what string) {
	e.t.Helper()
	if rec.Code != status {
		e.t.Fatalf("%s: status %d, want %d: %s", what, rec.Code, status, rec.Body)
	}
}

func TestGenerateAndManageAPIs(t *testing.T) {
	e := newEnv(t, nil)
	editor := e.token("ed", "/workspaces/acme/editor")
	viewer := e.token("vi", "/workspaces/acme/viewer")
	e.dataset("ds-orders", "Orders", "orders", `[{"name":"id","type":"long"},{"name":"amount","type":"decimal","description":"in CAD"},{"name":"placed","type":"ts"}]`)

	rec, _ := e.do(viewer, "acme", "POST", "/api/apis", map[string]string{"datasetId": "ds-orders"})
	e.want(rec, 403, "viewer generating")

	rec, got := e.do(editor, "acme", "POST", "/api/apis", map[string]string{"datasetId": "ds-orders"})
	e.want(rec, 201, "editor generating")
	id := got["id"].(string)
	if got["slug"] != "orders" || fmt.Sprint(got["primaryKey"]) != "[id]" || got["createdBy"] != "ed" {
		t.Errorf("definition = %v", got)
	}
	cols := got["columns"].([]any)
	if len(cols) != 3 || cols[1].(map[string]any)["type"] != "numeric(12,2)" || cols[1].(map[string]any)["description"] != "in CAD" {
		t.Errorf("columns = %v (types must come from the table, descriptions from the catalog)", cols)
	}

	rec, _ = e.do(editor, "acme", "POST", "/api/apis", map[string]string{"datasetId": "ds-orders"})
	e.want(rec, 409, "second API for the same dataset")

	rec, got = e.do(viewer, "acme", "GET", "/api/apis/"+id+"/schema", nil)
	e.want(rec, 200, "schema")
	if sdl, _ := got["sdl"].(string); !strings.Contains(sdl, "amount: Decimal") || !strings.Contains(sdl, "row(id: BigInt!): Row") {
		t.Errorf("sdl = %s", sdl)
	}
	rec, got = e.do(viewer, "acme", "GET", "/api/apis/"+id+"/openapi", nil)
	e.want(rec, 200, "openapi")
	if got["openapi"] != "3.1.0" || got["paths"].(map[string]any)["/rows/{key}"] == nil {
		t.Errorf("openapi = %v", got)
	}

	rec, got = e.do(viewer, "acme", "GET", "/api/apis", nil)
	e.want(rec, 200, "viewer listing")
	if n := len(got["items"].([]any)); n != 1 {
		t.Errorf("listed %d APIs", n)
	}
	rec, got = e.do(e.token("gl", "/workspaces/globex/owner"), "globex", "GET", "/api/apis", nil)
	e.want(rec, 200, "another workspace listing")
	if n := len(got["items"].([]any)); n != 0 {
		t.Errorf("globex sees %d of acme's APIs", n)
	}
	rec, _ = e.do(e.token("gl", "/workspaces/globex/owner"), "globex", "GET", "/api/apis/"+id, nil)
	e.want(rec, 404, "another workspace reading acme's API by id")

	// A second dataset with the same name gets a distinct slug.
	if _, err := e.pool.Exec(context.Background(), `CREATE TABLE orders_eu (id int)`); err != nil {
		t.Fatal(err)
	}
	e.dataset("ds-orders-eu", "Orders", "orders_eu", `[]`)
	rec, got = e.do(editor, "acme", "POST", "/api/apis", map[string]string{"datasetId": "ds-orders-eu"})
	e.want(rec, 201, "same name, other dataset")
	if got["slug"] != "orders-2" || len(got["primaryKey"].([]any)) != 0 {
		t.Errorf("second definition = %v", got)
	}

	// Regeneration picks up a changed table (catalog schema empty, so nothing to mismatch).
	if _, err := e.pool.Exec(context.Background(), `ALTER TABLE orders_eu ADD COLUMN country text`); err != nil {
		t.Fatal(err)
	}
	rec, got = e.do(editor, "acme", "POST", "/api/apis/"+got["id"].(string)+"/regenerate", nil)
	e.want(rec, 200, "regenerate")
	if len(got["columns"].([]any)) != 2 || got["slug"] != "orders-2" {
		t.Errorf("regenerated = %v", got)
	}

	rec, _ = e.do(editor, "acme", "DELETE", "/api/apis/"+id, nil)
	e.want(rec, 204, "delete")
	rec, _ = e.do(editor, "acme", "DELETE", "/api/apis/"+id, nil)
	e.want(rec, 404, "delete twice")
}

func TestGenerateRefusals(t *testing.T) {
	e := newEnv(t, nil)
	editor := e.token("ed", "/workspaces/acme/editor")
	e.catalog["ds-file"] = `{"id":"ds-file","name":"Raw","location":{"backendId":"b","path":"p"},"schema":[]}`
	e.dataset("ds-missing-table", "Ghost", "no_such_table", `[]`)
	e.dataset("ds-drift", "Drift", "orders", `[{"name":"id"},{"name":"amount"},{"name":"currency"}]`)
	if _, err := e.pool.Exec(context.Background(), `CREATE TABLE blobs (b bytea)`); err != nil {
		t.Fatal(err)
	}
	e.dataset("ds-blobs", "Blobs", "blobs", `[]`)

	for _, tc := range []struct {
		dataset string
		status  int
	}{
		{"ds-file", 422},          // v0 is postgres only
		{"ds-missing-table", 422}, // the catalog names a table that isn't there
		{"ds-nope", 404},          // not in the catalog (or not visible to the caller)
		{"ds-blobs", 422},         // no column of a type the API can serve
	} {
		rec, _ := e.do(editor, "acme", "POST", "/api/apis", map[string]string{"datasetId": tc.dataset})
		e.want(rec, tc.status, tc.dataset)
	}

	rec, got := e.do(editor, "acme", "POST", "/api/apis", map[string]string{"datasetId": "ds-drift"})
	e.want(rec, 409, "catalog/table mismatch")
	if s := fmt.Sprint(got["mismatches"]); !strings.Contains(s, "currency") || !strings.Contains(s, "placed") {
		t.Errorf("mismatches = %s, want currency (catalog only) and placed (table only)", s)
	}

	rec, _ = e.do(editor, "acme", "POST", "/api/apis", map[string]any{"datasetId": "x", "extra": 1})
	e.want(rec, 400, "unknown body field")
}

func TestGenerateWithoutADataSource(t *testing.T) {
	e := newEnv(t, source.Unavailable{})
	e.dataset("ds-orders", "Orders", "orders", `[]`)
	rec, _ := e.do(e.token("ed", "/workspaces/acme/editor"), "acme", "POST", "/api/apis", map[string]string{"datasetId": "ds-orders"})
	e.want(rec, 503, "production has no data source until the sidecar path is built")
}

func TestKeys(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	owner := e.token("ow", "/workspaces/acme/owner")
	editor := e.token("ed", "/workspaces/acme/editor")
	viewer := e.token("vi", "/workspaces/acme/viewer")
	e.dataset("ds-orders", "Orders", "orders", `[]`)
	if rec, _ := e.do(editor, "acme", "POST", "/api/apis", map[string]string{"datasetId": "ds-orders"}); rec.Code != 201 {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body)
	}

	for _, path := range []string{"/api/keys"} {
		rec, _ := e.do(viewer, "acme", "GET", path, nil)
		e.want(rec, 403, "viewer listing keys")
	}
	rec, _ := e.do(viewer, "acme", "POST", "/api/keys", map[string]any{"name": "x", "datasetIds": []string{"ds-orders"}})
	e.want(rec, 403, "viewer issuing a key")
	for _, body := range []map[string]any{
		{"name": "", "datasetIds": []string{"ds-orders"}},
		{"name": "x", "datasetIds": []string{}},
		{"name": "x", "datasetIds": []string{"ds-orders", "ds-without-api"}},
		{"name": strings.Repeat("n", 101), "datasetIds": []string{"ds-orders"}},
	} {
		rec, _ := e.do(editor, "acme", "POST", "/api/keys", body)
		e.want(rec, 400, fmt.Sprint(body))
	}

	rec, got := e.do(editor, "acme", "POST", "/api/keys", map[string]any{"name": " nightly export ", "datasetIds": []string{"ds-orders", "ds-orders"}})
	e.want(rec, 201, "issue")
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the response carrying a key's secret is cacheable")
	}
	secret, _ := got["secret"].(string)
	keyID := got["id"].(string)
	if !strings.HasPrefix(secret, keys.Prefix+keyID+"_") || got["name"] != "nightly export" || got["createdByName"] != "ed-name" {
		t.Fatalf("issued = %v", got)
	}
	if _, leaked := got["secretHash"]; leaked {
		t.Error("the hash is serialized")
	}

	// Shown once: nothing afterwards carries the secret or its hash.
	rec, _ = e.do(owner, "acme", "GET", "/api/keys", nil)
	e.want(rec, 200, "list keys")
	if strings.Contains(rec.Body.String(), secret[len(keys.Prefix)+14:]) || strings.Contains(strings.ToLower(rec.Body.String()), "hash") {
		t.Fatal("the key list exposes the secret or its hash")
	}
	var stored []byte
	if err := e.pool.QueryRow(ctx, `SELECT secret_hash FROM api_keys WHERE id = $1`, keyID).Scan(&stored); err != nil || bytes.Contains(stored, []byte(secret[len(keys.Prefix)+14:])) {
		t.Fatalf("stored = %x, %v", stored, err)
	}

	p, err := e.keys.Verify(ctx, secret)
	if err != nil || p.Workspace != "acme" || p.Owner != "ed" || !p.Allows("ds-orders") || p.Allows("ds-other") || fmt.Sprint(p.DatasetIDs) != "[ds-orders]" {
		t.Fatalf("Verify = %+v, %v", p, err)
	}
	for _, bad := range []string{secret[:len(secret)-2] + "AA", strings.Replace(secret, keyID, "aaaaaaaaaaaaa", 1), "nonsense", ""} {
		if _, err := e.keys.Verify(ctx, bad); err != keys.ErrUnauthenticated {
			t.Errorf("Verify(%q) = %v", bad, err)
		}
	}

	// Another workspace can't see or revoke it.
	globex := e.token("gl", "/workspaces/globex/owner")
	rec, _ = e.do(globex, "globex", "POST", "/api/keys/"+keyID+"/revoke", nil)
	e.want(rec, 404, "revoke from another workspace")
	if _, err := e.keys.Verify(ctx, secret); err != nil {
		t.Fatal("a foreign revoke attempt revoked the key")
	}

	rec, got = e.do(owner, "acme", "POST", "/api/keys/"+keyID+"/revoke", nil)
	e.want(rec, 200, "revoke")
	if got["revokedAt"] == nil || got["revokedBy"] != "ow" {
		t.Errorf("revoked = %v", got)
	}
	if _, err := e.keys.Verify(ctx, secret); err != keys.ErrUnauthenticated {
		t.Errorf("revoked key still verifies: %v", err)
	}
	first := got["revokedAt"]
	rec, got = e.do(editor, "acme", "POST", "/api/keys/"+keyID+"/revoke", nil)
	e.want(rec, 200, "revoke twice")
	if got["revokedAt"] != first || got["revokedBy"] != "ow" {
		t.Errorf("a second revoke rewrote the first: %v", got)
	}
}

// Deleting an API takes it out of every key's scope, so recreating an API for the same dataset
// doesn't silently re-enable keys issued for the old one.
func TestDeletingAnAPIDropsItFromKeys(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	editor := e.token("ed", "/workspaces/acme/editor")
	e.dataset("ds-orders", "Orders", "orders", `[]`)
	_, api := e.do(editor, "acme", "POST", "/api/apis", map[string]string{"datasetId": "ds-orders"})
	_, k := e.do(editor, "acme", "POST", "/api/keys", map[string]any{"name": "k", "datasetIds": []string{"ds-orders"}})
	secret := k["secret"].(string)

	rec, _ := e.do(editor, "acme", "DELETE", "/api/apis/"+api["id"].(string), nil)
	e.want(rec, 204, "delete")
	rec, _ = e.do(editor, "acme", "POST", "/api/apis", map[string]string{"datasetId": "ds-orders"})
	e.want(rec, 201, "recreate")
	p, err := e.keys.Verify(ctx, secret)
	if err != nil || p.Allows("ds-orders") {
		t.Errorf("old key after delete+recreate: %+v, %v (want valid but scoped to nothing)", p, err)
	}
}

func TestNoVerifierMeans503(t *testing.T) {
	h := api.NewHandler(api.Deps{Verifier: func() auth.TokenVerifier { return nil }})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/apis", nil))
	if rec.Code != 503 {
		t.Errorf("status %d", rec.Code)
	}
}
