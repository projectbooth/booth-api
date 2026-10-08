package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	var ready atomic.Bool
	h := NewRouter(Deps{DB: fakeDB{}, Ready: &ready})
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("before migrations: /healthz = %d, want 503", rec.Code)
	}
	ready.Store(true)
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthy DB: /healthz = %d", rec.Code)
	}
	if rec := get(t, NewRouter(Deps{DB: fakeDB{err: errors.New("down")}, Ready: &ready}), "/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("DB down: /healthz = %d, want 503", rec.Code)
	}
}

func TestLivezIgnoresDatabase(t *testing.T) {
	if rec := get(t, NewRouter(Deps{DB: fakeDB{err: errors.New("down")}}), "/livez"); rec.Code != http.StatusOK {
		t.Errorf("/livez = %d with the DB down; a restart can't fix that, so liveness must not fail", rec.Code)
	}
}

// Only /api/* reaches the management API. Generated endpoints aren't mounted until core's public
// routes exist (ADR 0101), so their paths must 404 rather than fall through to anything.
func TestRoutes(t *testing.T) {
	var hit string
	api := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { hit = r.URL.Path })
	h := NewRouter(Deps{DB: fakeDB{}, API: api})
	if get(t, h, "/api/keys"); hit != "/api/keys" {
		t.Errorf("/api/keys not routed to the management API (hit %q)", hit)
	}
	for _, p := range []string{"/", "/v1/orders/rows", "/public/v1/x", "/graphql"} {
		hit = ""
		if rec := get(t, h, p); rec.Code != http.StatusNotFound || hit != "" {
			t.Errorf("%s = %d (hit %q), want 404", p, rec.Code, hit)
		}
	}
}
