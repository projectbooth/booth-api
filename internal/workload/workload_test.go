package workload

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-api/internal/source"
)

func TestMint(t *testing.T) {
	var got Request
	var auth string
	status, body := 200, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields() // core's handler does the same
		if err := dec.Decode(&got); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if status != 200 {
			http.Error(w, body, status)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	m := &Minter{URL: srv.URL + "/api/internal/workload-tokens", Credential: "wl.api.cred"}
	ctx := context.Background()

	exp := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	body = `{"token":"jwt","tokenType":"Bearer","expiresAt":"` + exp.Format(time.RFC3339) + `","role":"viewer"}`
	tok, err := m.Mint(ctx, ReadRequest("acme", "sub-alice"))
	if err != nil {
		t.Fatal(err)
	}
	if tok.JWT != "jwt" || !tok.ExpiresAt.Equal(exp) || tok.Role != "viewer" {
		t.Errorf("token = %+v", tok)
	}
	if auth != "Bearer wl.api.cred" {
		t.Errorf("Authorization = %q", auth)
	}
	if got != (Request{Workspace: "acme", Subject: "apikeys:acme", RoleCeiling: "viewer", Owner: "sub-alice"}) {
		t.Errorf("request = %+v", got)
	}

	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"owner has no access": {403, "the run's owner has no current access to that workspace", source.ErrOwnerNoAccess},
		"module not entitled": {403, "this module is not entitled to mint workload tokens", ErrNotEntitled},
		"bad credential":      {401, "invalid minting credential", ErrNotEntitled},
	} {
		status, body = tc.status, tc.body
		if _, err := m.Mint(ctx, ReadRequest("acme", "sub-alice")); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	status, body = 503, "workload identity is temporarily unavailable"
	if _, err := m.Mint(ctx, ReadRequest("acme", "x")); err == nil || errors.Is(err, source.ErrOwnerNoAccess) || !strings.Contains(err.Error(), "503") {
		t.Errorf("503: %v", err)
	}
	status, body = 200, `{"token":"jwt","expiresAt":"`+exp.Format(time.RFC3339)+`","role":"owner"}`
	if _, err := m.Mint(ctx, ReadRequest("acme", "x")); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("a role above the ceiling was accepted: %v", err)
	}
	status, body = 200, `{"token":""}`
	if _, err := m.Mint(ctx, ReadRequest("acme", "x")); err == nil {
		t.Error("a malformed response was accepted")
	}
}

// A write token (ADR 0117): apiwrite:<api-id>, ceiling editor. Core caps an owner at editor; an
// owner demoted to viewer can no longer back a write, which reads as "no access".
func TestMintWrite(t *testing.T) {
	var got Request
	role := "editor"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		exp := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
		_, _ = w.Write([]byte(`{"token":"jwt","expiresAt":"` + exp + `","role":"` + role + `"}`))
	}))
	defer srv.Close()
	m := &Minter{URL: srv.URL, Credential: "c"}
	ctx := context.Background()
	if _, err := m.Mint(ctx, WriteRequest("acme", "api-1", "sub-alice")); err != nil {
		t.Fatal(err)
	}
	if got != (Request{Workspace: "acme", Subject: "apiwrite:api-1", RoleCeiling: "editor", Owner: "sub-alice"}) {
		t.Errorf("request = %+v", got)
	}
	role = "viewer"
	if _, err := m.Mint(ctx, WriteRequest("acme", "api-1", "sub-alice")); !errors.Is(err, source.ErrOwnerNoAccess) {
		t.Errorf("viewer-capped write token: err = %v, want ErrOwnerNoAccess", err)
	}
	role = "owner"
	if _, err := m.Mint(ctx, WriteRequest("acme", "api-1", "sub-alice")); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("a role above the editor ceiling was accepted: %v", err)
	}
}
