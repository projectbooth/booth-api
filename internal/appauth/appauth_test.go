package appauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		sub, module string
		groups      []string
		ok          bool
	}{
		"an app":                       {"streamlit:acme:a2b3c4", "streamlit", []string{"/workspaces/acme/viewer"}, true},
		"another module minted it":     {"streamlit:acme:a2b3c4", "pipeline", []string{"/workspaces/acme/viewer"}, false},
		"booth-api's own write token":  {"apiwrite:api-1", "api", []string{"/workspaces/acme/editor"}, false},
		"streamlit's catalog identity": {"streamlit:acme:sources", "streamlit", []string{"/workspaces/acme/viewer"}, false},
		"workspace mismatch":           {"streamlit:acme:a2b3c4", "streamlit", []string{"/workspaces/globex/viewer"}, false},
		"two groups":                   {"streamlit:acme:a2b3c4", "streamlit", []string{"/workspaces/acme/viewer", "/workspaces/acme/owner"}, false},
		"no groups":                    {"streamlit:acme:a2b3c4", "streamlit", nil, false},
		"a person":                     {"5d3c-uuid", "", []string{"/workspaces/acme/owner"}, false},
	} {
		c, err := Check(tc.sub, tc.module, tc.groups)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
		}
		if tc.ok && (c.Subject != tc.sub || c.Workspace != "acme" || c.AppID != "a2b3c4" || c.Role != "viewer") {
			t.Errorf("%s: caller = %+v", name, c)
		}
	}
}

func TestValidPrincipal(t *testing.T) {
	if !ValidPrincipal("streamlit:acme:app-1", "acme") {
		t.Error("an app in the workspace was refused")
	}
	for _, p := range []string{"streamlit:globex:app-1", "streamlit:acme:sources", "key:abc", "apiwrite:x", "streamlit:acme:a:b"} {
		if ValidPrincipal(p, "acme") {
			t.Errorf("%q accepted as an acme app principal", p)
		}
	}
}

// issuer is a minimal stand-in for core's workload issuer: discovery, JWKS, RS256 tokens.
type issuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newIssuer(t *testing.T) *issuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &issuer{key: key}
	mux := http.NewServeMux()
	iss.srv = httptest.NewServer(mux)
	t.Cleanup(iss.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": iss.srv.URL, "jwks_uri": iss.srv.URL + "/.well-known/jwks.json",
			"id_token_signing_alg_values_supported": []string{"RS256"}, "response_types_supported": []string{"id_token"}, "subject_types_supported": []string{"public"}})
	})
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	return iss
}

func (iss *issuer) token(t *testing.T, issuerURL, sub, module string, groups []string) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: iss.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: issuerURL, Subject: sub, Audience: jwt.Audience{"booth-design"},
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(5 * time.Minute))}).
		Claims(map[string]any{"groups": groups, ModuleClaim: module}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVerifier(t *testing.T) {
	iss := newIssuer(t)
	ctx := context.Background()
	v, err := NewVerifier(ctx, Config{IssuerURL: iss.srv.URL, ClientID: "booth-design", RequireAudience: true})
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.Verify(ctx, iss.token(t, iss.srv.URL, "streamlit:acme:app1", "streamlit", []string{"/workspaces/acme/viewer"}))
	if err != nil || c.Subject != "streamlit:acme:app1" || c.Workspace != "acme" {
		t.Fatalf("app token: %+v, %v", c, err)
	}
	if _, err := v.Verify(ctx, iss.token(t, "https://keycloak.example/realms/booth", "streamlit:acme:app1", "streamlit", []string{"/workspaces/acme/viewer"})); err == nil {
		t.Error("a token from another issuer was accepted")
	}
	if _, err := v.Verify(ctx, iss.token(t, iss.srv.URL, "job:42", "pipeline", []string{"/workspaces/acme/viewer"})); !errors.Is(err, ErrNotAnApp) {
		t.Errorf("a pipeline token: err = %v, want ErrNotAnApp", err)
	}
	other := newIssuer(t) // right issuer URL, wrong key
	if _, err := v.Verify(ctx, other.token(t, iss.srv.URL, "streamlit:acme:app1", "streamlit", []string{"/workspaces/acme/viewer"})); err == nil {
		t.Error("a token signed by another key was accepted")
	}
}

func TestCheckService(t *testing.T) {
	s, err := CheckService("streamlit:acme:grants", "streamlit", []string{"/workspaces/acme/viewer"})
	if err != nil || s.Workspace != "acme" || s.Subject != "streamlit:acme:grants" {
		t.Fatalf("grants service = %+v, %v", s, err)
	}
	for name, tc := range map[string]struct {
		sub, module string
		groups      []string
	}{
		"an app":             {"streamlit:acme:budget", "streamlit", []string{"/workspaces/acme/viewer"}},
		"another module":     {"streamlit:acme:grants", "pipeline", []string{"/workspaces/acme/viewer"}},
		"workspace mismatch": {"streamlit:acme:grants", "streamlit", []string{"/workspaces/globex/viewer"}},
		"no groups":          {"streamlit:acme:grants", "streamlit", nil},
	} {
		if _, err := CheckService(tc.sub, tc.module, tc.groups); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The grants service is never an app: it can't write, and can't be granted.
	if _, err := Check("streamlit:acme:grants", "streamlit", []string{"/workspaces/acme/viewer"}); err == nil {
		t.Error("the grants service token was accepted as an app")
	}
	if ValidPrincipal("streamlit:acme:grants", "acme") {
		t.Error("the grants service is a valid grant principal")
	}
}
