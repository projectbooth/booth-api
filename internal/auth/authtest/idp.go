// Package authtest is a minimal but real OIDC provider for tests — a discovery document, a JWKS
// endpoint and an RSA signing key — so token verification is exercised through the same go-oidc
// code path production uses rather than a stub. Same approach as booth-storage's auth tests.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// IdP is a running fake identity provider.
type IdP struct {
	URL string
	Key *rsa.PrivateKey
}

// New starts an IdP for the duration of the test.
func New(t testing.TB) *IdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &IdP{Key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": idp.URL, "jwks_uri": idp.URL + "/jwks",
			"authorization_endpoint": idp.URL + "/auth", "token_endpoint": idp.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	idp.URL = srv.URL
	return idp
}

// Token describes a token to mint. Zero values mean: this IdP as issuer, one hour's expiry,
// this IdP's key, no audience, no groups claim.
type Token struct {
	Issuer   string
	Subject  string
	Audience string
	Expiry   time.Duration
	SignWith *rsa.PrivateKey
	// Groups, if non-nil, is emitted as the claim named GroupsClaim (default "groups").
	Groups      any
	GroupsClaim string
	// Extra claims, e.g. preferred_username.
	Extra map[string]any
}

// Mint signs a token.
func (idp *IdP) Mint(t testing.TB, o Token) string {
	t.Helper()
	if o.Issuer == "" {
		o.Issuer = idp.URL
	}
	if o.Expiry == 0 {
		o.Expiry = time.Hour
	}
	key := o.SignWith
	if key == nil {
		key = idp.Key
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.Claims{Issuer: o.Issuer, Subject: o.Subject, IssuedAt: jwt.NewNumericDate(time.Now()), Expiry: jwt.NewNumericDate(time.Now().Add(o.Expiry))}
	if o.Audience != "" {
		claims.Audience = jwt.Audience{o.Audience}
	}
	b := jwt.Signed(signer).Claims(claims)
	if o.Groups != nil {
		name := o.GroupsClaim
		if name == "" {
			name = "groups"
		}
		b = b.Claims(map[string]any{name: o.Groups})
	}
	if o.Extra != nil {
		b = b.Claims(o.Extra)
	}
	raw, err := b.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
