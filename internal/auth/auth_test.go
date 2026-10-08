package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/projectbooth/booth-api/internal/auth"
	"github.com/projectbooth/booth-api/internal/auth/authtest"
)

func TestVerifier(t *testing.T) {
	idp := authtest.New(t)
	other := authtest.New(t) // e.g. booth-core's workload issuer: a real, but untrusted, issuer
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ctx := context.Background()

	lax, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-api"})
	if err != nil {
		t.Fatal(err)
	}
	strict, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-api", RequireAudience: true})
	if err != nil {
		t.Fatal(err)
	}
	custom, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-api", GroupsClaim: "roles"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		v          *auth.Verifier
		raw        string
		wantErr    bool
		wantGroups int
	}{
		{"valid", lax, idp.Mint(t, authtest.Token{Subject: "alice", Groups: []string{"/workspaces/acme/owner"}}), false, 1},
		{"expired", lax, idp.Mint(t, authtest.Token{Subject: "alice", Expiry: -time.Hour}), true, 0},
		{"signed by an unknown key", lax, idp.Mint(t, authtest.Token{Subject: "alice", SignWith: otherKey}), true, 0},
		{"claims another issuer", lax, idp.Mint(t, authtest.Token{Subject: "alice", Issuer: "https://evil.example"}), true, 0},
		// A genuine token from a different real issuer (as a workload token would be) is refused:
		// this API trusts the OIDC provider only.
		{"another real issuer", lax, other.Mint(t, authtest.Token{Subject: "job:1", Groups: []string{"/workspaces/acme/owner"}}), true, 0},
		{"missing subject", lax, idp.Mint(t, authtest.Token{}), true, 0},
		{"not a JWT", lax, "garbage", true, 0},
		{"audience required and matching", strict, idp.Mint(t, authtest.Token{Subject: "alice", Audience: "booth-api"}), false, 0},
		{"audience required but wrong", strict, idp.Mint(t, authtest.Token{Subject: "alice", Audience: "someone-else"}), true, 0},
		{"groups claim of the wrong shape fails closed", lax, idp.Mint(t, authtest.Token{Subject: "alice", Groups: "/workspaces/acme/owner"}), false, 0},
		{"configurable groups claim", custom, idp.Mint(t, authtest.Token{Subject: "alice", Groups: []string{"/workspaces/acme/owner"}, GroupsClaim: "roles"}), false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := tc.v.Verify(ctx, tc.raw)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && len(c.Groups) != tc.wantGroups {
				t.Errorf("groups = %v", c.Groups)
			}
		})
	}
}

func TestRoleInWorkspace(t *testing.T) {
	groups := []string{"/workspaces/acme/viewer", "/workspaces/acme/owner", "/workspaces/globex/editor", "/workspaces/Bad/owner", "/other/x", "/workspaces/acme/admin"}
	if r := auth.RoleInWorkspace(groups, "acme"); r != auth.RoleOwner {
		t.Errorf("acme = %q, want the highest (owner)", r)
	}
	if r := auth.RoleInWorkspace(groups, "initech"); r != "" {
		t.Errorf("initech = %q, want none", r)
	}
}

func TestMiddleware(t *testing.T) {
	idp := authtest.New(t)
	v, err := auth.NewVerifier(context.Background(), auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-api"})
	if err != nil {
		t.Fatal(err)
	}
	var got auth.Identity
	h := auth.Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = auth.FromContext(r.Context())
	}))
	editor := idp.Mint(t, authtest.Token{Subject: "ed", Groups: []string{"/workspaces/acme/editor"}})
	owner := idp.Mint(t, authtest.Token{Subject: "ow", Groups: []string{"/workspaces/acme/owner"}})

	cases := []struct {
		name, token, workspace, forwardedRole string
		status                                int
		role                                  auth.Role
	}{
		{"no token", "", "acme", "", 401, ""},
		{"bad token", "x.y.z", "acme", "", 401, ""},
		{"no workspace header", owner, "", "", 400, ""},
		{"no role in that workspace", owner, "globex", "", 403, ""},
		{"token role used when no header", editor, "acme", "", 200, auth.RoleEditor},
		// ADR 0041: a header claiming more than the token grants is forged — rejected, not trimmed.
		{"forged owner header on an editor token", editor, "acme", "owner", 403, ""},
		{"header may narrow", owner, "acme", "viewer", 200, auth.RoleViewer},
		{"unknown header value grants nothing", owner, "acme", "superuser", 200, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got = auth.Identity{}
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.workspace != "" {
				req.Header.Set(auth.HeaderBoothWorkspace, tc.workspace)
			}
			if tc.forwardedRole != "" {
				req.Header.Set(auth.HeaderBoothRole, tc.forwardedRole)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if tc.status == 200 && got.Role != tc.role {
				t.Errorf("role = %q, want %q", got.Role, tc.role)
			}
		})
	}
}

func TestCanWrite(t *testing.T) {
	for role, want := range map[auth.Role]bool{auth.RoleOwner: true, auth.RoleEditor: true, auth.RoleViewer: false, "": false} {
		if got := (auth.Identity{Role: role}).CanWrite(); got != want {
			t.Errorf("CanWrite(%q) = %v, want %v", role, got, want)
		}
	}
}
