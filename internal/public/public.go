// Package public serves the generated endpoints on booth-api's public route (ADR 0101): core's
// gateway proxies /modules/api/public/v1/... here as /v1/... with no platform login, and this
// package authenticates the caller by API key and decides 401/403 itself.
//
//	GET  /v1/{slug}/rows, /v1/{slug}/rows/{key}, /v1/{slug}/openapi.json   (internal/rest)
//	POST /v1/{slug}/graphql, GET /v1/{slug}/graphql?query=...             (internal/gql)
//
// Identity comes from the key and nothing else. X-Booth-Workspace, X-Booth-Role and
// X-Booth-Identity are never read on this path (core strips them, ADR 0101 item 4, and a caller
// reaching the module directly could set anything): the workspace is the key's, the dataset scope
// is the key's, and the database identity is a workload token owned by the key's creator
// (ADR 0103).
package public

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/gql"
	"github.com/projectbooth/booth-api/internal/keys"
	"github.com/projectbooth/booth-api/internal/rest"
	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/store"
	"github.com/projectbooth/booth-api/internal/table"
)

// Prefix is the module-relative path prefix declared in the manifest's publicRoutes.
const Prefix = "/v1/"

// Keys is the slice of internal/keys this package needs.
type Keys interface {
	Verify(ctx context.Context, presented string) (keys.Principal, error)
}

// APIs is the slice of internal/store this package needs.
type APIs interface {
	GetAPIBySlug(ctx context.Context, workspace, slug string) (apidef.Definition, error)
	TouchKey(ctx context.Context, id string) error
}

// Deps is what the handler needs.
type Deps struct {
	Keys Keys
	APIs APIs
	// Pools hands out a workspace database pool under the key creator's identity (ADR 0103).
	Pools  source.Pools
	Limits gql.Limits
}

// Handler serves every generated API.
type Handler struct {
	d     Deps
	mu    sync.Mutex
	cache map[string]*engines // by API id + generatedAt, so a regenerate is picked up at once
}

type engines struct {
	model table.Model
	gql   *gql.Engine
}

// maxCached bounds the engine cache; past it the cache is simply cleared (rebuilding an engine is
// a schema parse, cheap next to the query it serves).
const maxCached = 256

// maxBody bounds a GraphQL POST body: the query limit plus room for variables.
const maxBody = 64 << 10

// NewHandler builds the handler; mount it at Prefix.
func NewHandler(d Deps) *Handler { return &Handler{d: d, cache: map[string]*engines{}} }

var slugRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Work on the escaped path so an encoded "/" inside a REST key stays inside that segment. Core
	// decodes and cleans public paths before forwarding (its decision 0016), so through the
	// gateway such a key arrives already split; it can only be reached by filtering on the column.
	rest0 := strings.TrimPrefix(r.URL.EscapedPath(), strings.TrimSuffix(Prefix, "/"))
	slug, rest1, _ := strings.Cut(strings.TrimPrefix(rest0, "/"), "/")
	sub := "/" + rest1
	isGraphQL := sub == "/graphql" || sub == "/graphql/"
	if !slugRE.MatchString(slug) {
		h.deny(w, isGraphQL, http.StatusNotFound, "not found")
		return
	}

	// 1. Who: the key, and only the key.
	presented, ok := bearer(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="booth-api"`)
		h.deny(w, isGraphQL, http.StatusUnauthorized, "an API key is required: Authorization: Bearer booth_ak_...")
		return
	}
	p, err := h.d.Keys.Verify(r.Context(), presented)
	if errors.Is(err, keys.ErrUnauthenticated) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="booth-api", error="invalid_token"`)
		h.deny(w, isGraphQL, http.StatusUnauthorized, "invalid or revoked API key")
		return
	}
	if err != nil {
		h.internal(w, isGraphQL, err)
		return
	}

	// 2. What: an API in the key's own workspace, inside the key's scope.
	def, err := h.d.APIs.GetAPIBySlug(r.Context(), p.Workspace, slug)
	if errors.Is(err, store.ErrNotFound) {
		h.deny(w, isGraphQL, http.StatusNotFound, "no API named "+slug)
		return
	}
	if err != nil {
		h.internal(w, isGraphQL, err)
		return
	}
	if !p.Allows(def.DatasetID) {
		h.deny(w, isGraphQL, http.StatusForbidden, "this key isn't scoped to the "+slug+" API")
		return
	}
	go h.touch(p.KeyID)

	e, err := h.engines(def)
	if err != nil {
		h.internal(w, isGraphQL, err)
		return
	}

	// The schema documents are served without touching the workspace database.
	if sub == "/openapi.json" {
		(&rest.Handler{Model: e.model, Limits: h.d.Limits.Limits}).ServeHTTP(w, withPath(r, sub))
		return
	}

	// 3. Data, as the key's creator (ADR 0103).
	db, err := h.d.Pools.Pool(r.Context(), p.Workspace, p.Owner)
	switch {
	case errors.Is(err, source.ErrOwnerNoAccess):
		h.deny(w, isGraphQL, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, source.ErrUnavailable):
		h.deny(w, isGraphQL, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		h.internal(w, isGraphQL, err)
		return
	}

	if isGraphQL {
		h.graphql(w, r, e.gql, db)
		return
	}
	(&rest.Handler{Model: e.model, Limits: h.d.Limits.Limits, DB: db, OnInternalError: logError}).ServeHTTP(w, withPath(r, sub))
}

func (h *Handler) graphql(w http.ResponseWriter, r *http.Request, e *gql.Engine, db table.DB) {
	var req gql.Request
	switch r.Method {
	case http.MethodPost:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			h.deny(w, true, http.StatusRequestEntityTooLarge, "request body over 64 KiB")
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			h.deny(w, true, http.StatusBadRequest, "the body must be a JSON object {query, operationName, variables}")
			return
		}
	case http.MethodGet:
		q := r.URL.Query()
		req.Query, req.OperationName = q.Get("query"), q.Get("operationName")
		if v := q.Get("variables"); v != "" {
			if err := json.Unmarshal([]byte(v), &req.Variables); err != nil {
				h.deny(w, true, http.StatusBadRequest, "variables must be a JSON object")
				return
			}
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		h.deny(w, true, http.StatusMethodNotAllowed, "use POST (or GET) for GraphQL queries; this API is read-only")
		return
	}
	resp := e.Execute(r.Context(), db, req)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) engines(def apidef.Definition) (*engines, error) {
	key := def.ID + "@" + def.GeneratedAt.String()
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.cache[key]; ok {
		return e, nil
	}
	m := table.NewModel(def)
	g, err := gql.New(m, h.d.Limits)
	if err != nil {
		return nil, err
	}
	g.OnInternalError = logError
	if len(h.cache) >= maxCached {
		h.cache = map[string]*engines{}
	}
	e := &engines{model: m, gql: g}
	h.cache[key] = e
	return e, nil
}

func (h *Handler) touch(keyID string) {
	if err := h.d.APIs.TouchKey(context.Background(), keyID); err != nil {
		log.Printf("public: recording key %s use: %v", keyID, err)
	}
}

// withPath hands a sub-handler the request with the API's own path (/rows, /rows/{key}, ...).
func withPath(r *http.Request, escaped string) *http.Request {
	r2 := r.Clone(r.Context())
	u := *r.URL
	u.RawPath = escaped
	if p, err := url.PathUnescape(escaped); err == nil {
		u.Path = p
	} else {
		u.Path, u.RawPath = escaped, ""
	}
	r2.URL = &u
	return r2
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// deny writes an error in the shape the caller's protocol expects.
func (h *Handler) deny(w http.ResponseWriter, isGraphQL bool, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if isGraphQL {
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]string{"message": msg}}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}})
}

func (h *Handler) internal(w http.ResponseWriter, isGraphQL bool, err error) {
	logError(err)
	h.deny(w, isGraphQL, http.StatusInternalServerError, "internal error")
}

func logError(err error) { log.Printf("public: %v", err) }
