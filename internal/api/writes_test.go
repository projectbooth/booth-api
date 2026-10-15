package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/projectbooth/booth-api/internal/api"
	"github.com/projectbooth/booth-api/internal/apis"
	"github.com/projectbooth/booth-api/internal/auth"
	"github.com/projectbooth/booth-api/internal/auth/authtest"
	"github.com/projectbooth/booth-api/internal/writes/writestest"
)

// The write-management routes (docs/contracts/app-writes.md): owners only, except reading an
// API's write settings.
func TestWriteManagement(t *testing.T) {
	w := writestest.New(t)
	def := w.API("entries")
	idp := authtest.New(t)
	v, err := auth.NewVerifier(context.Background(), auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-api"})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, idp: idp, h: api.NewHandler(api.Deps{
		Verifier: func() auth.TokenVerifier { return v },
		APIs:     apis.Service{Store: w.Store},
		Writes:   w.Service,
	})}
	owner := e.token("alice", "/workspaces/acme/owner")
	editor := e.token("ed", "/workspaces/acme/editor")
	viewer := e.token("vi", "/workspaces/acme/viewer")
	base := "/api/apis/" + def.ID + "/writes"
	settings := map[string]any{"operations": []string{"insert"}, "columns": []string{"amount", "entered_by"}}

	rec, got := e.do(viewer, "acme", "GET", base, nil)
	e.want(rec, 200, "a viewer reads the write settings")
	if got["writes"] != nil || got["hasPrimaryKey"] != true || len(got["columns"].([]any)) != 6 {
		t.Errorf("settings = %v", got)
	}
	for _, c := range got["columns"].([]any) {
		c := c.(map[string]any)
		if c["column"] == "id" && (c["writable"] != false || !strings.Contains(c["reason"].(string), "primary-key")) {
			t.Errorf("id = %v", c)
		}
		if c["column"] == "entered_by" && c["required"] != true {
			t.Errorf("entered_by = %v", c)
		}
	}

	rec, _ = e.do(editor, "acme", "PUT", base, settings)
	e.want(rec, 403, "an editor turning writes on")
	rec, got = e.do(owner, "acme", "PUT", base, map[string]any{"operations": []string{"insert"}, "columns": []string{"amount"}})
	e.want(rec, 422, "settings missing a required column")
	if !strings.Contains(fmtJSON(got["problems"]), "entered_by") {
		t.Errorf("problems = %v", got)
	}
	rec, got = e.do(owner, "acme", "PUT", base, settings)
	e.want(rec, 200, "an owner turns writes on")
	if got["writes"].(map[string]any)["version"] != float64(1) {
		t.Errorf("saved = %v", got)
	}

	grant := map[string]any{"apiId": def.ID, "principal": "streamlit:acme:budget"}
	rec, _ = e.do(editor, "acme", "POST", "/api/write-grants", grant)
	e.want(rec, 403, "an editor granting")
	rec, _ = e.do(owner, "acme", "POST", "/api/write-grants", map[string]any{"apiId": def.ID, "principal": "key:abc"})
	e.want(rec, 422, "a write key (not built)")
	rec, _ = e.do(owner, "acme", "POST", "/api/write-grants", map[string]any{"apiId": def.ID, "principal": "streamlit:globex:x"})
	e.want(rec, 400, "another workspace's app")
	rec, got = e.do(owner, "acme", "POST", "/api/write-grants", grant)
	e.want(rec, 201, "an owner grants an app")
	if got["principal"] != "streamlit:acme:budget" || fmtJSON(got["operations"]) != `["insert"]` || got["grantedBy"] != "alice" {
		t.Errorf("grant = %v", got)
	}
	gid := got["id"].(string)
	rec, _ = e.do(viewer, "acme", "GET", "/api/write-grants", nil)
	e.want(rec, 403, "a viewer listing grants")
	rec, got = e.do(owner, "acme", "GET", "/api/write-grants?principal=streamlit:acme:budget", nil)
	e.want(rec, 200, "list grants")
	if len(got["items"].([]any)) != 1 {
		t.Errorf("grants = %v", got)
	}
	rec, got = e.do(owner, "acme", "POST", "/api/write-grants/revoke-principal", map[string]string{"principal": "streamlit:acme:budget"})
	e.want(rec, 200, "revoke every grant for an app")
	if got["revoked"] != float64(1) {
		t.Errorf("revoke-principal = %v", got)
	}
	rec, got = e.do(owner, "acme", "POST", "/api/write-grants/"+gid+"/revoke", nil)
	e.want(rec, 200, "revoke an already revoked grant")
	if got["revokedAt"] == nil {
		t.Errorf("grant = %v", got)
	}

	// Deleting a writable API: owners only, and it turns writes off first.
	rec, _ = e.do(editor, "acme", "DELETE", "/api/apis/"+def.ID, nil)
	e.want(rec, 409, "an editor deleting a writable API")
	rec, got = e.do(owner, "acme", "DELETE", base, nil)
	e.want(rec, 200, "writes off")
	if got["databaseRevoked"] != true {
		t.Errorf("off = %v", got)
	}
	rec, _ = e.do(owner, "acme", "POST", "/api/write-grants", grant)
	e.want(rec, 409, "a grant with writes off")

	// An app's workload token (core's issuer, not the people's) is refused on management routes.
	core := authtest.New(t)
	appTok := core.Mint(t, authtest.Token{Subject: "streamlit:acme:budget", Groups: []string{"/workspaces/acme/viewer"}})
	rec, _ = e.do(appTok, "acme", "GET", base, nil)
	e.want(rec, 401, "an app token on a management route")
	rec, _ = e.do(appTok, "acme", "POST", "/api/write-grants", grant)
	e.want(rec, 401, "an app token granting itself")
}

func fmtJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
