package appwrite_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/projectbooth/booth-api/internal/appauth"
	"github.com/projectbooth/booth-api/internal/appwrite"
	"github.com/projectbooth/booth-api/internal/auth"
	"github.com/projectbooth/booth-api/internal/gql"
	"github.com/projectbooth/booth-api/internal/ratelimit"
	"github.com/projectbooth/booth-api/internal/writes/writestest"
)

// fakeVerifier maps a token to a caller: "app:<ws>:<id>" is an app, "pipeline" a non-app token,
// anything else invalid.
type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, raw string) (appauth.Caller, error) {
	if raw == "pipeline" {
		return appauth.Caller{}, appauth.ErrNotAnApp
	}
	rest, ok := strings.CutPrefix(raw, "app:")
	if !ok {
		return appauth.Caller{}, context.DeadlineExceeded
	}
	ws, id, _ := strings.Cut(rest, ":")
	return appauth.Check("streamlit:"+ws+":"+id, "streamlit", []string{"/workspaces/" + ws + "/viewer"})
}

type harness struct {
	*writestest.Env
	h     http.Handler
	apiID string
}

func newHarness(t *testing.T, ops, cols []string) *harness {
	e := writestest.New(t)
	def := e.API("entries")
	ctx := context.Background()
	alice := auth.Identity{Subject: "alice", Workspace: "acme", Role: auth.RoleOwner}
	if _, err := e.Service.SaveSettings(ctx, alice, def.ID, ops, cols); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Service.Grant(ctx, alice, def.ID, "streamlit:acme:budget", ops, nil); err != nil {
		t.Fatal(err)
	}
	h := appwrite.NewHandler(appwrite.Deps{
		Verifier: func() appauth.TokenVerifier { return fakeVerifier{} },
		Writes:   &e.Service, Limits: gql.DefaultLimits,
	})
	return &harness{Env: e, h: h, apiID: def.ID}
}

func (h *harness) do(method, path, token, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

func want(t *testing.T, rec *httptest.ResponseRecorder, status int, what string) map[string]any {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", what, rec.Code, status, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

const app = "app:acme:budget"

func TestAuthentication(t *testing.T) {
	h := newHarness(t, []string{"insert"}, []string{"amount", "entered_by"})
	body := `{"data":{"amount":"1","entered_by":"sam"}}`
	want(t, h.do("POST", "/app/v1/entries/rows", "", body, nil), 401, "no token")
	want(t, h.do("POST", "/app/v1/entries/rows", "garbage", body, nil), 401, "an invalid token")
	want(t, h.do("POST", "/app/v1/entries/rows", "pipeline", body, nil), 403, "a workload token that isn't an app's")
	// The workspace is the token's: another workspace's app doesn't find acme's API, whatever
	// headers say.
	want(t, h.do("POST", "/app/v1/entries/rows", "app:globex:budget", body, map[string]string{"X-Booth-Workspace": "acme", "X-Workspace": "acme"}), 404, "globex app with forged headers")
	// An app with no grant.
	out := want(t, h.do("POST", "/app/v1/entries/rows", "app:acme:other", body, nil), 403, "an app with no grant")
	if out["error"].(map[string]any)["code"] != "not_granted" {
		t.Errorf("error = %v", out)
	}

	unconfigured := appwrite.NewHandler(appwrite.Deps{Verifier: func() appauth.TokenVerifier { return nil }})
	rec := httptest.NewRecorder()
	unconfigured.ServeHTTP(rec, httptest.NewRequest("POST", "/app/v1/entries/rows", strings.NewReader(body)))
	if rec.Code != 503 {
		t.Errorf("no workload issuer configured: %d", rec.Code)
	}
}

// App tokens are refused on reads: nothing here serves a GET.
func TestNoReads(t *testing.T) {
	h := newHarness(t, []string{"insert"}, []string{"amount", "entered_by"})
	for _, p := range []string{"/app/v1/entries/rows", "/app/v1/entries/rows/1", "/app/v1/entries/graphql", "/app/v1/entries/openapi.json"} {
		if rec := h.do("GET", p, app, "", nil); rec.Code != 405 && rec.Code != 404 {
			t.Errorf("GET %s: %d", p, rec.Code)
		}
	}
	out := want(t, h.do("POST", "/app/v1/entries/graphql", app, `{"query":"{ _writeOnly }"}`, nil), 200, "a GraphQL query")
	if !strings.Contains(mustJSON(out), "only mutations") {
		t.Errorf("query = %v", out)
	}
}

func TestRESTWrites(t *testing.T) {
	h := newHarness(t, []string{"insert", "update"}, []string{"amount", "note", "entered_by"})
	rec := h.do("POST", "/app/v1/entries/rows", app, `{"data":{"amount":"12.50","note":"lunch","entered_by":"sam"}}`, map[string]string{appwrite.HeaderAppUser: "sam@example"})
	out := want(t, rec, 201, "insert")
	data := out["data"].(map[string]any)
	if data["id"] != "1" || data["amount"] != "12.50" || out["version"] == "" || rec.Header().Get("Location") != "/modules/api/app/v1/entries/rows/1" {
		t.Errorf("insert = %v, Location %q", out, rec.Header().Get("Location"))
	}
	etag := rec.Header().Get("ETag")
	if etag != `"`+out["version"].(string)+`"` {
		t.Errorf("ETag = %q", etag)
	}
	// Keys appear in column order, every column.
	if !strings.HasPrefix(rec.Body.String(), `{"data":{"id":"1","amount":"12.50","note":"lunch","entered_by":"sam","booked":`) {
		t.Errorf("body = %s", rec.Body)
	}

	out = want(t, h.do("PATCH", "/app/v1/entries/rows/1", app, `{"data":{"note":"dinner"}}`, map[string]string{"If-Match": etag}), 200, "update with If-Match")
	if out["data"].(map[string]any)["note"] != "dinner" {
		t.Errorf("update = %v", out)
	}
	want(t, h.do("PATCH", "/app/v1/entries/rows/1", app, `{"data":{"note":"again"}}`, map[string]string{"If-Match": etag}), 412, "stale If-Match")
	want(t, h.do("PATCH", "/app/v1/entries/rows?key[id]=1", app, `{"data":{"note":"by query key"}}`, nil), 200, "update by key[...]")
	want(t, h.do("PATCH", "/app/v1/entries/rows?key[nope]=1", app, `{"data":{"note":"x"}}`, nil), 400, "a key that isn't the primary key")
	want(t, h.do("DELETE", "/app/v1/entries/rows/1", app, "", nil), 403, "delete isn't enabled")
	want(t, h.do("PATCH", "/app/v1/entries/rows/99", app, `{"data":{"note":"x"}}`, nil), 404, "no such row")

	out = want(t, h.do("POST", "/app/v1/entries/rows", app, `{"data":{"amount":"1","secret":"x"}}`, nil), 422, "a column not writable")
	e := out["error"].(map[string]any)
	if e["code"] == "" || len(e["fields"].([]any)) != 2 {
		t.Errorf("422 = %v", out)
	}
	want(t, h.do("POST", "/app/v1/entries/rows", app, `{"data":{"amount":"1"},"extra":1}`, nil), 400, "unknown body field")
	want(t, h.do("POST", "/app/v1/entries/rows", app, `{"data":{"amount":"1"}}{"data":{}}`, nil), 400, "two rows")
	want(t, h.do("POST", "/app/v1/entries/rows?x=1", app, `{"data":{}}`, nil), 400, "a parameter on insert")
	big := `{"data":{"note":"` + strings.Repeat("x", 70<<10) + `"}}`
	want(t, h.do("POST", "/app/v1/entries/rows", app, big, nil), 413, "a body over 64 KiB")
}

func TestIdempotencyHeader(t *testing.T) {
	h := newHarness(t, []string{"insert"}, []string{"amount", "entered_by"})
	hdr := map[string]string{"Idempotency-Key": "abc"}
	first := want(t, h.do("POST", "/app/v1/entries/rows", app, `{"data":{"amount":"3","entered_by":"sam"}}`, hdr), 201, "first")
	rec := h.do("POST", "/app/v1/entries/rows", app, `{"data":{"amount":"3","entered_by":"sam"}}`, hdr)
	again := want(t, rec, 201, "replay")
	if mustJSON(first) != mustJSON(again) || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay = %v (header %q), first %v", again, rec.Header().Get("Idempotent-Replayed"), first)
	}
	want(t, h.do("POST", "/app/v1/entries/rows", app, `{"data":{"amount":"4","entered_by":"sam"}}`, hdr), 422, "same key, other body")
}

func TestGraphQLWrites(t *testing.T) {
	h := newHarness(t, []string{"insert", "update"}, []string{"amount", "note", "entered_by"})
	out := want(t, h.do("POST", "/app/v1/entries/graphql", app,
		`{"query":"mutation($d: RowInsert!) { insertRow(data: $d) { row { id note } version } }","variables":{"d":{"amount":"5","note":"gql","entered_by":"sam"}}}`, nil), 200, "insertRow")
	got := mustJSON(out)
	if !strings.Contains(got, `"row":{"id":"1","note":"gql"}`) || !strings.Contains(got, `"version":"`) {
		t.Errorf("insertRow = %s", got)
	}
	out = want(t, h.do("POST", "/app/v1/entries/graphql", app, `{"query":"mutation { insertRow(data: {amount: \"-1\", entered_by: \"sam\"}) { version } }"}`, nil), 200, "a refused insertRow")
	if !strings.Contains(mustJSON(out), `"code":"constraint"`) {
		t.Errorf("refusal = %v", out)
	}
	want(t, h.do("POST", "/app/v1/entries/graphql", "app:acme:other", `{"query":"mutation { insertRow(data: {amount: \"1\"}) { version } }"}`, nil), 403, "GraphQL with no grant")
}

func TestRateLimited(t *testing.T) {
	h := newHarness(t, []string{"insert"}, []string{"amount", "entered_by"})
	h.Service.Limiter = ratelimit.New(0.001, 1)
	// The handler holds a pointer to the service, so the new limiter applies.
	want(t, h.do("POST", "/app/v1/entries/rows", app, `{"data":{"amount":"1","entered_by":"sam"}}`, nil), 201, "first")
	rec := h.do("POST", "/app/v1/entries/rows", app, `{"data":{"amount":"1","entered_by":"sam"}}`, nil)
	want(t, rec, 429, "over the rate")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
