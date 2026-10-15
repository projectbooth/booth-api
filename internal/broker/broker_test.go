package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTableWriteAndRevoke(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	status, body := 201, `{"leaseId":"l1","kind":"postgres","expiresAt":"2026-10-15T01:00:00Z","scope":{"workspace":"acme","write":{"grant":"api:a1"}},"credential":{"password":"s3cret"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path {
			http.NotFound(w, r)
			return
		}
		hdr = r.Header.Clone()
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := &Client{CoreURL: srv.URL + "/"}
	ctx := context.Background()

	scope := json.RawMessage(`{"workspace":"acme","write":{"grant":"api:a1","tables":[]}}`)
	if err := c.TableWrite(ctx, "tok", "acme", scope, "streamlit:acme:budget"); err != nil {
		t.Fatal(err)
	}
	if hdr.Get("Authorization") != "Bearer tok" || hdr.Get("X-Workspace") != "acme" {
		t.Errorf("headers = %v", hdr)
	}
	if got["kind"] != "postgres" || got["access"] != "tablewrite" || got["onBehalfOf"] != "streamlit:acme:budget" || got["options"] != nil {
		t.Errorf("request = %v", got)
	}

	// The revoke request: tables [] with options.revoke; the 201 carries credential: null.
	body = `{"leaseId":"l2","kind":"postgres","expiresAt":"2026-10-15T00:00:00Z","scope":{"workspace":"acme","write":{"grant":"api:a1","tables":[],"revoked":true}},"credential":null}`
	if err := c.RevokeTableWrite(ctx, "tok", "acme", "api:a1", "alice"); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"options":{"revoke":true}`) || !strings.Contains(string(b), `"scope":{"workspace":"acme","write":{"grant":"api:a1","tables":[]}}`) {
		t.Errorf("revoke request = %s", b)
	}
	body = `{"leaseId":"l2","scope":{"write":{"grant":"api:a1"}},"credential":null}`
	if err := c.RevokeTableWrite(ctx, "tok", "acme", "api:a1", "alice"); err == nil {
		t.Error("a revoke answer without scope.write.revoked was accepted")
	}

	status, body = 422, `{"error":"scope_not_supported","message":"finance.entries: column \"x\" doesn't exist"}`
	err := c.TableWrite(ctx, "tok", "acme", scope, "")
	var se *ScopeError
	if !errors.As(err, &se) || !strings.Contains(se.Message, "finance.entries") {
		t.Errorf("422: %v", err)
	}
	status, body = 403, `{"error":"forbidden"}`
	if err := c.TableWrite(ctx, "tok", "acme", scope, ""); !errors.Is(err, ErrRefused) {
		t.Errorf("403: %v", err)
	}
	status, body = 502, `{"credential":{"password":"s3cret"}}`
	if err := c.TableWrite(ctx, "tok", "acme", scope, ""); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("502: %v (must not echo the body)", err)
	}
}
