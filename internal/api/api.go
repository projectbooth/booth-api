// Package api is booth-api's management API, mounted at /api/ and reached from the module's UI
// through booth-core's gateway (/modules/api/api/...). Every route needs a person's verified OIDC
// token (internal/auth, ADR 0041); reading the workspace's APIs is open to any role, while
// generating or deleting an API and anything to do with keys needs editor or owner (the
// ADR 0038/0048 write precedent).
//
// The generated endpoints themselves are not here: they are served on core's public routes
// (ADR 0101) and authenticated by key, once core's change lands.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/apis"
	"github.com/projectbooth/booth-api/internal/auth"
	"github.com/projectbooth/booth-api/internal/catalog"
	"github.com/projectbooth/booth-api/internal/gql"
	"github.com/projectbooth/booth-api/internal/keys"
	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/store"
)

const maxBody = 64 << 10

// Deps is what the handler needs.
type Deps struct {
	// Verifier returns the token verifier, or nil while OIDC discovery hasn't succeeded (every
	// request then gets 503, never an unauthenticated pass).
	Verifier func() auth.TokenVerifier
	APIs     apis.Service
	Keys     keys.Service
}

// NewHandler builds the /api/ router.
func NewHandler(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			v := d.Verifier()
			if v == nil {
				auth.WriteError(w, http.StatusServiceUnavailable, "token verification is not configured yet")
				return
			}
			auth.Middleware(v)(next).ServeHTTP(w, req)
		})
	})
	h := handlers{d}
	r.Get("/api/apis", h.listAPIs)
	r.Get("/api/apis/{id}", h.getAPI)
	r.Get("/api/apis/{id}/schema", h.schema)
	r.With(writer).Post("/api/apis", h.generate)
	r.With(writer).Post("/api/apis/{id}/regenerate", h.regenerate)
	r.With(writer).Delete("/api/apis/{id}", h.deleteAPI)
	r.With(writer).Get("/api/keys", h.listKeys)
	r.With(writer).Post("/api/keys", h.issueKey)
	r.With(writer).Post("/api/keys/{id}/revoke", h.revokeKey)
	return r
}

// writer refuses viewers. Key listing sits behind it too: a viewer can't issue or revoke keys, so
// it has no use for the list of who holds which.
func writer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, _ := auth.FromContext(r.Context()); !id.CanWrite() {
			auth.WriteError(w, http.StatusForbidden, "managing APIs and keys needs the editor or owner role")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type handlers struct{ d Deps }

func who(r *http.Request) auth.Identity {
	id, _ := auth.FromContext(r.Context())
	return id
}

func (h handlers) listAPIs(w http.ResponseWriter, r *http.Request) {
	items, err := h.d.APIs.List(r.Context(), who(r).Workspace)
	respond(w, r, http.StatusOK, map[string]any{"items": items}, err)
}

func (h handlers) getAPI(w http.ResponseWriter, r *http.Request) {
	d, err := h.d.APIs.Get(r.Context(), who(r).Workspace, chi.URLParam(r, "id"))
	respond(w, r, http.StatusOK, d, err)
}

func (h handlers) schema(w http.ResponseWriter, r *http.Request) {
	s, err := h.d.APIs.GraphQLSchema(r.Context(), who(r).Workspace, chi.URLParam(r, "id"))
	respond(w, r, http.StatusOK, s, err)
}

func (h handlers) generate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DatasetID string `json:"datasetId"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.DatasetID == "" {
		auth.WriteError(w, http.StatusBadRequest, "datasetId is required")
		return
	}
	d, err := h.d.APIs.Generate(r.Context(), who(r), in.DatasetID)
	respond(w, r, http.StatusCreated, d, err)
}

func (h handlers) regenerate(w http.ResponseWriter, r *http.Request) {
	d, err := h.d.APIs.Regenerate(r.Context(), who(r), chi.URLParam(r, "id"))
	respond(w, r, http.StatusOK, d, err)
}

func (h handlers) deleteAPI(w http.ResponseWriter, r *http.Request) {
	err := h.d.APIs.Delete(r.Context(), who(r).Workspace, chi.URLParam(r, "id"))
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	respond(w, r, 0, nil, err)
}

func (h handlers) listKeys(w http.ResponseWriter, r *http.Request) {
	items, err := h.d.Keys.List(r.Context(), who(r).Workspace)
	respond(w, r, http.StatusOK, map[string]any{"items": items}, err)
}

func (h handlers) issueKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string   `json:"name"`
		DatasetIDs []string `json:"datasetIds"`
	}
	if !decode(w, r, &in) {
		return
	}
	id := who(r)
	k, err := h.d.Keys.Issue(r.Context(), id.Workspace, in.Name, in.DatasetIDs, id.Subject, id.DisplayName)
	// The secret is in this one response and nowhere else; tell every cache so.
	w.Header().Set("Cache-Control", "no-store")
	respond(w, r, http.StatusCreated, k, err)
}

func (h handlers) revokeKey(w http.ResponseWriter, r *http.Request) {
	k, err := h.d.Keys.Revoke(r.Context(), who(r).Workspace, chi.URLParam(r, "id"), who(r).Subject)
	respond(w, r, http.StatusOK, k, err)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		auth.WriteError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

// respond writes v with status on success, or maps err to a status and a message safe to show.
func respond(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
		return
	}
	var mm *apidef.MismatchError
	switch {
	case errors.As(err, &mm):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": mm.Error(), "mismatches": mm.Mismatches})
	case errors.Is(err, store.ErrNotFound):
		auth.WriteError(w, http.StatusNotFound, "not found")
	case errors.Is(err, catalog.ErrNotFound):
		auth.WriteError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, catalog.ErrDenied):
		auth.WriteError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrExists):
		auth.WriteError(w, http.StatusConflict, err.Error())
	case errors.Is(err, keys.ErrInvalid):
		auth.WriteError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, apis.ErrUnsupportedFormat), errors.Is(err, apis.ErrNoColumns), errors.Is(err, source.ErrTableNotFound),
		errors.Is(err, gql.ErrNoFields):
		auth.WriteError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, source.ErrUnavailable):
		auth.WriteError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, context.Canceled):
		// Client went away; nothing useful to write.
	default:
		log.Printf("api: %s %s: %v", r.Method, r.URL.Path, err)
		auth.WriteError(w, http.StatusInternalServerError, "internal error")
	}
}
