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
	var got mintRequest
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
	tok, err := m.Mint(ctx, "acme", "sub-alice")
	if err != nil {
		t.Fatal(err)
	}
	if tok.JWT != "jwt" || !tok.ExpiresAt.Equal(exp) || tok.Role != "viewer" {
		t.Errorf("token = %+v", tok)
	}
	if auth != "Bearer wl.api.cred" {
		t.Errorf("Authorization = %q", auth)
	}
	if got != (mintRequest{Workspace: "acme", Subject: "apikeys:acme", RoleCeiling: "viewer", Owner: "sub-alice"}) {
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
		if _, err := m.Mint(ctx, "acme", "sub-alice"); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	status, body = 503, "workload identity is temporarily unavailable"
	if _, err := m.Mint(ctx, "acme", "x"); err == nil || errors.Is(err, source.ErrOwnerNoAccess) || !strings.Contains(err.Error(), "503") {
		t.Errorf("503: %v", err)
	}
	status, body = 200, `{"token":"jwt","expiresAt":"`+exp.Format(time.RFC3339)+`","role":"owner"}`
	if _, err := m.Mint(ctx, "acme", "x"); err == nil || !strings.Contains(err.Error(), "above") {
		t.Errorf("a role above the ceiling was accepted: %v", err)
	}
	status, body = 200, `{"token":""}`
	if _, err := m.Mint(ctx, "acme", "x"); err == nil {
		t.Error("a malformed response was accepted")
	}
}
