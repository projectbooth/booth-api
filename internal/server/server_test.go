package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	if rec := get(t, NewRouter(Deps{DB: fakeDB{}}), "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthy DB: /healthz = %d", rec.Code)
	}
	if rec := get(t, NewRouter(Deps{DB: fakeDB{err: errors.New("down")}}), "/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("DB down: /healthz = %d, want 503", rec.Code)
	}
}

func TestLivezIgnoresDatabase(t *testing.T) {
	if rec := get(t, NewRouter(Deps{DB: fakeDB{err: errors.New("down")}}), "/livez"); rec.Code != http.StatusOK {
		t.Errorf("/livez = %d with the DB down; a restart can't fix that, so liveness must not fail", rec.Code)
	}
}

// Nothing beyond the probes is served until the key path and data layer are built (ADR 0100,
// README.md): an unbuilt route must 404, not fall through to something permissive.
func TestNoOtherRoutes(t *testing.T) {
	h := NewRouter(Deps{DB: fakeDB{}})
	for _, p := range []string{"/", "/api/keys", "/v1/datasets/x", "/graphql"} {
		if rec := get(t, h, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, rec.Code)
		}
	}
}
