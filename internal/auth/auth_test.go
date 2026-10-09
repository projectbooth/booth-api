package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// ADR 0108 key-fetch override: keys from a plain-http JWKS URL, the issuer an https URL nothing can
// reach. A token from that issuer verifies, discovery is never contacted, and `iss` is still
// compared exactly (a token signed with the same key but naming another issuer is refused).
func TestVerifierJWKSOverride(t *testing.T) {
	idp := authtest.New(t)
	const issuer = "https://booth.home.arpa.invalid/realms/booth"
	ctx := context.Background()
	v, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: issuer, ClientID: "booth-api", JWKSURL: idp.URL + "/jwks"})
	if err != nil {
		t.Fatalf("NewVerifier with an unreachable issuer and a JWKS URL: %v", err)
	}

	c, err := v.Verify(ctx, idp.Mint(t, authtest.Token{Issuer: issuer, Subject: "alice", Groups: []string{"/workspaces/acme/owner"}}))
	if err != nil || c.Subject != "alice" || len(c.Groups) != 1 {
		t.Fatalf("valid token: %+v, %v", c, err)
	}
	for name, iss := range map[string]string{
		"another https issuer":             "https://wrong.invalid/realms/booth",
		"the JWKS server's own address":    idp.URL,
		"the issuer with a trailing slash": issuer + "/",
	} {
		if _, err := v.Verify(ctx, idp.Mint(t, authtest.Token{Issuer: iss, Subject: "alice"})); err == nil {
			t.Errorf("%s: a token naming %q verified; iss must match %q exactly", name, iss, issuer)
		}
	}
	if n := idp.DiscoveryHits.Load(); n != 0 {
		t.Errorf("discovery was requested %d times with the override set", n)
	}

	// The audience policy is unchanged by the override.
	strict, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: issuer, ClientID: "booth-api", JWKSURL: idp.URL + "/jwks", RequireAudience: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Verify(ctx, idp.Mint(t, authtest.Token{Issuer: issuer, Subject: "alice", Audience: "someone-else"})); err == nil {
		t.Error("audience not enforced with the override set")
	}
	if _, err := strict.Verify(ctx, idp.Mint(t, authtest.Token{Issuer: issuer, Subject: "alice", Audience: "booth-api"})); err != nil {
		t.Errorf("matching audience: %v", err)
	}
}

// Without the override, discovery is used exactly as before.
func TestVerifierWithoutOverrideUsesDiscovery(t *testing.T) {
	idp := authtest.New(t)
	if _, err := auth.NewVerifier(context.Background(), auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-api"}); err != nil {
		t.Fatal(err)
	}
	if idp.DiscoveryHits.Load() == 0 {
		t.Error("discovery wasn't used with no JWKS URL set")
	}
}

func TestNewVerifierRejectsJWKSURLWithoutIssuer(t *testing.T) {
	idp := authtest.New(t)
	if _, err := auth.NewVerifier(context.Background(), auth.OIDCConfig{ClientID: "booth-api", JWKSURL: idp.URL + "/jwks"}); err == nil {
		t.Error("a JWKS URL without an issuer was accepted")
	}
}

// The issuer and the key source are logged once, at construction; tokens never are.
func TestNewVerifierLogsIssuerAndKeySourceOnce(t *testing.T) {
	idp := authtest.New(t)
	const issuer = "https://booth.home.arpa.invalid/realms/booth"
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	v, err := auth.NewVerifier(context.Background(), auth.OIDCConfig{IssuerURL: issuer, ClientID: "booth-api", JWKSURL: idp.URL + "/jwks"})
	if err != nil {
		t.Fatal(err)
	}
	tok := idp.Mint(t, authtest.Token{Issuer: issuer, Subject: "alice"})
	_, _ = v.Verify(context.Background(), tok)
	out := buf.String()
	if strings.Count(out, "oidc: verifying tokens") != 1 || !strings.Contains(out, "issuer="+issuer) || !strings.Contains(out, "keys-from="+idp.URL+"/jwks") {
		t.Errorf("log = %q", out)
	}
	if strings.Contains(out, tok) {
		t.Error("a token was logged")
	}
}
