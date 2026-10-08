// Package server assembles booth-api's HTTP surface: the probes, and the management API at /api/
// (internal/api) that the module's UI calls through booth-core's gateway. The generated endpoints
// are served on core's public routes (ADR 0101) and are mounted here once core's change lands.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
)

// Pinger is the slice of the module's own database /healthz needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps is what the router needs.
type Deps struct {
	DB Pinger
	// Ready flips true once the store's migrations have applied; until then /healthz reports
	// unready, so a pod is never routed to before its tables exist.
	Ready *atomic.Bool
	// API is the management API (internal/api), mounted at /api/. Nil mounts nothing.
	API http.Handler
}

// NewRouter builds the HTTP handler.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	// /livez never touches the database: restarting the pod can't fix a Postgres outage.
	r.Get("/livez", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// /healthz is readiness and what booth-core polls (the manifest's healthCheckPath). The
	// module's own database holds the API keys, so without it nothing it serves can work.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := d.DB.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "database unreachable"})
			return
		}
		if d.Ready != nil && !d.Ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	if d.API != nil {
		r.Handle("/api/*", d.API)
	}
	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
