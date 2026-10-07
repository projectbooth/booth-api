// Package server assembles booth-api's HTTP surface. At scaffold stage it serves only the two
// probes; the module's own management API (/api/*, OIDC-verified, ADR 0041) and the generated
// endpoints (API-key-authenticated, ADR 0100) are mounted here once they are built. The generated
// endpoints wait on a contract note for how core's gateway reaches them without an OIDC session
// (README.md, "Not built yet").
package server

import (
	"context"
	"encoding/json"
	"net/http"
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
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
