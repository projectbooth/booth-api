// Package appwrite serves the app write routes (ADR 0117; docs/contracts/app-writes.md), reached
// through booth-core's ordinary gateway as /modules/api/app/v1/... and mounted here at /app/v1/:
//
//	POST   /app/v1/{slug}/rows                 {"data": {...}}        insert  -> 201 {"data", "version"}
//	PATCH  /app/v1/{slug}/rows/{key}           {"data": {...}}        update  -> 200 {"data", "version"}
//	DELETE /app/v1/{slug}/rows/{key}                                  delete  -> 200 {"data", "version"}
//	PATCH|DELETE /app/v1/{slug}/rows?key[field]=value&...             composite keys, or keys with "/"
//	POST   /app/v1/{slug}/graphql              insertRow / updateRow / deleteRow mutations
//
// Only a booth-streamlit app's own workload token is accepted (internal/appauth), and nothing here
// reads: GET is 405. The workspace is the token's. Headers: Idempotency-Key (optional),
// If-Match: "<version>" (optional, update and delete), X-Booth-App-User (audited as reported by
// booth-streamlit's gate, never trusted).
package appwrite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/appauth"
	"github.com/projectbooth/booth-api/internal/gql"
	"github.com/projectbooth/booth-api/internal/table"
	"github.com/projectbooth/booth-api/internal/writes"
)

// Prefix is where the handler is mounted.
const Prefix = "/app/v1/"

// HeaderAppUser is the person booth-streamlit's gate says is using the app.
const HeaderAppUser = "X-Booth-App-User"

// Writes is the slice of internal/writes this package needs.
type Writes interface {
	Resolve(ctx context.Context, c appauth.Caller, slug string) (writes.Target, error)
	Execute(ctx context.Context, t writes.Target, w writes.AppWrite) (writes.Outcome, error)
	LogResolveRefusal(ctx context.Context, c appauth.Caller, slug, op string, err error)
}

// Deps is what the handler needs.
type Deps struct {
	// Verifier returns the app-token verifier, or nil while it isn't configured or discovery
	// hasn't succeeded yet (every request is then 503).
	Verifier func() appauth.TokenVerifier
	Writes   Writes
	Limits   gql.Limits
}

// Handler serves the app write routes.
type Handler struct {
	d     Deps
	mu    sync.Mutex
	cache map[string]*gql.WriteEngine
}

// NewHandler builds the handler; mount it at Prefix.
func NewHandler(d Deps) *Handler { return &Handler{d: d, cache: map[string]*gql.WriteEngine{}} }

var (
	slugRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	keyRE  = regexp.MustCompile(`^key\[([^\[\]]+)\]$`)
)

const maxCached = 256

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest0 := strings.TrimPrefix(r.URL.EscapedPath(), strings.TrimSuffix(Prefix, "/"))
	slug, rest1, _ := strings.Cut(strings.TrimPrefix(rest0, "/"), "/")
	sub := "/" + strings.TrimSuffix(rest1, "/")
	isGraphQL := sub == "/graphql"
	if !slugRE.MatchString(slug) {
		h.fail(w, isGraphQL, &table.WriteError{Status: 404, Code: table.CodeNotFound, Msg: "not found"})
		return
	}

	// 1. Who: an app token verified against core's workload issuer, and nothing else.
	var v appauth.TokenVerifier
	if h.d.Verifier != nil {
		v = h.d.Verifier()
	}
	if v == nil {
		h.fail(w, isGraphQL, &table.WriteError{Status: 503, Code: table.CodeUnavailable, Msg: "app writes are not configured (oidc.workloadIssuerUrl), or core's workload issuer hasn't answered yet"})
		return
	}
	token, ok := bearer(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="booth-api-app"`)
		h.fail(w, isGraphQL, &table.WriteError{Status: 401, Code: "unauthenticated", Msg: "an app workload token is required"})
		return
	}
	caller, err := v.Verify(r.Context(), token)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="booth-api-app", error="invalid_token"`)
		if errors.Is(err, appauth.ErrNotAnApp) {
			h.fail(w, isGraphQL, &table.WriteError{Status: 403, Code: "not_an_app", Msg: "only a booth-streamlit app's workload token may write here"})
			return
		}
		h.fail(w, isGraphQL, &table.WriteError{Status: 401, Code: "unauthenticated", Msg: "invalid token"})
		return
	}

	// 2. What: the request's shape, before anything is looked up.
	var req table.WriteRequest
	var mu *gql.Mutation
	if isGraphQL {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			h.fail(w, true, &table.WriteError{Status: 405, Code: table.CodeInvalidRequest, Msg: "use POST: this endpoint takes mutations only"})
			return
		}
	} else {
		var we *table.WriteError
		req, we = parseREST(w, r, sub)
		if we != nil {
			h.fail(w, false, we)
			return
		}
	}

	// 3. Which API, and the app's live grant on it.
	target, err := h.d.Writes.Resolve(r.Context(), caller, slug)
	if err != nil {
		op := req.Op
		if isGraphQL {
			op = "graphql"
		}
		h.d.Writes.LogResolveRefusal(r.Context(), caller, slug, op, err)
		h.writeErr(w, isGraphQL, err)
		return
	}
	if v, ok := req.Key[pathKey]; ok {
		if len(target.Model.PK) != 1 {
			h.fail(w, false, &table.WriteError{Status: 404, Code: table.CodeNotFound,
				Msg: "this table has no single-column primary key; name the row with key[field]=value for each key field"})
			return
		}
		req.Key = map[string]any{target.Model.PK[0].Name: v}
	}
	idem := r.Header.Get("Idempotency-Key")
	if isGraphQL {
		eng, err := h.engine(target)
		if err != nil {
			h.writeErr(w, true, err)
			return
		}
		var greq gql.Request
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, writes.MaxBody))
		if err != nil {
			h.fail(w, true, &table.WriteError{Status: 413, Code: table.CodeInvalidRequest, Msg: "request body over 64 KiB"})
			return
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&greq); err != nil {
			h.fail(w, true, &table.WriteError{Status: 400, Code: table.CodeInvalidRequest, Msg: "the body must be a JSON object {query, operationName, variables}"})
			return
		}
		var resp *gql.Response
		mu, resp = eng.Parse(greq)
		if resp != nil {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		req = mu.Request
		if mu.IdempotencyKey != "" {
			idem = mu.IdempotencyKey
		}
	}

	// 4. The write.
	out, err := h.d.Writes.Execute(r.Context(), target, writes.AppWrite{
		Caller: caller, ReportedUser: reportedUser(r), Request: req, IdempotencyKey: idem,
	})
	if err != nil {
		h.writeErr(w, isGraphQL, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(out.Version))
	if out.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	if isGraphQL {
		writeJSON(w, http.StatusOK, mu.Respond(r.Context(), out.Row, out.Version))
		return
	}
	if req.Op == apidef.OpInsert {
		if loc := location(r, slug, target.Model, out.Row); loc != "" {
			w.Header().Set("Location", loc)
		}
	}
	var b bytes.Buffer
	b.WriteString(`{"data":`)
	writeRow(&b, target.Model, out.Row)
	b.WriteString(`,"version":`)
	v2, _ := json.Marshal(out.Version)
	b.Write(v2)
	b.WriteString("}\n")
	writeRaw(w, out.Status, b.Bytes())
}

// parseREST reads a REST write: the operation from the method, the key from the path or key[...]
// parameters, the data from {"data": {...}}, the version from If-Match.
func parseREST(w http.ResponseWriter, r *http.Request, sub string) (table.WriteRequest, *table.WriteError) {
	var req table.WriteRequest
	bad := func(format string, args ...any) (table.WriteRequest, *table.WriteError) {
		return req, &table.WriteError{Status: 400, Code: table.CodeInvalidRequest, Msg: fmt.Sprintf(format, args...)}
	}
	switch r.Method {
	case http.MethodPost:
		req.Op = apidef.OpInsert
	case http.MethodPatch:
		req.Op = apidef.OpUpdate
	case http.MethodDelete:
		req.Op = apidef.OpDelete
	default:
		w.Header().Set("Allow", "POST, PATCH, DELETE")
		return req, &table.WriteError{Status: 405, Code: table.CodeInvalidRequest, Msg: "this route only writes (POST inserts, PATCH updates, DELETE deletes); reads go through the app's own read path"}
	}
	q := r.URL.Query()
	switch {
	case sub == "/rows":
		if req.Op != apidef.OpInsert {
			req.Key = map[string]any{}
			for k, vals := range q {
				m := keyRE.FindStringSubmatch(k)
				if m == nil || len(vals) != 1 {
					return bad("unknown parameter %q (a key is key[field]=value, each once)", k)
				}
				req.Key[m[1]] = vals[0]
			}
			if len(req.Key) == 0 {
				return bad("name the row: /rows/{key}, or key[field]=value for each primary-key field")
			}
		} else if len(q) > 0 {
			return bad("an insert takes no parameters")
		}
	case strings.HasPrefix(sub, "/rows/") && !strings.Contains(sub[len("/rows/"):], "/"):
		if req.Op == apidef.OpInsert {
			return req, &table.WriteError{Status: 405, Code: table.CodeInvalidRequest, Msg: "insert with POST /rows"}
		}
		if len(q) > 0 {
			return bad("unknown parameter; a key in the path takes none")
		}
		key, err := url.PathUnescape(sub[len("/rows/"):])
		if err != nil {
			return bad("malformed key")
		}
		req.Key = map[string]any{pathKey: key}
	default:
		return req, &table.WriteError{Status: 404, Code: table.CodeNotFound, Msg: "not found"}
	}
	if v := r.Header.Get("If-Match"); v != "" {
		if req.Op == apidef.OpInsert {
			return bad("If-Match applies to update and delete only")
		}
		req.ExpectedVersion = strings.Trim(strings.TrimPrefix(v, "W/"), `"`)
	}
	if req.Op == apidef.OpDelete {
		if r.ContentLength > 0 {
			return bad("a delete takes no body")
		}
		return req, nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, writes.MaxBody))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := dec.Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return req, &table.WriteError{Status: 413, Code: table.CodeInvalidRequest, Msg: "request body over 64 KiB (one row per request)"}
		}
		return bad("the body must be {\"data\": {field: value, ...}}: %v", err)
	}
	if dec.More() {
		return bad("the body must be one JSON object (one row per request)")
	}
	if body.Data == nil {
		return bad("the body must be {\"data\": {field: value, ...}}")
	}
	req.Data = body.Data
	return req, nil
}

// pathKey stands for "the single primary-key field" until the model is known (resolveKey).
const pathKey = "\x00pk"

func (h *Handler) engine(t writes.Target) (*gql.WriteEngine, error) {
	k := t.Def.ID + "@" + t.Def.GeneratedAt.String() + "@" + strconv.FormatInt(t.Def.Writes.Version, 10)
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.cache[k]; ok {
		return e, nil
	}
	e, err := gql.NewWrite(t.Model, t.Def.Writes, h.d.Limits)
	if err != nil {
		return nil, err
	}
	if len(h.cache) >= maxCached {
		h.cache = map[string]*gql.WriteEngine{}
	}
	h.cache[k] = e
	return e, nil
}

func (h *Handler) writeErr(w http.ResponseWriter, isGraphQL bool, err error) {
	var we *table.WriteError
	var rl *writes.RateLimited
	switch {
	case errors.As(err, &we):
		h.fail(w, isGraphQL, we)
	case errors.As(err, &rl):
		w.Header().Set("Retry-After", strconv.Itoa(int(rl.Wait/time.Second)+1))
		h.fail(w, isGraphQL, &table.WriteError{Status: 429, Code: table.CodeRateLimited, Msg: rl.Error()})
	case errors.Is(err, table.ErrOutcomeUnknown):
		h.fail(w, isGraphQL, &table.WriteError{Status: 503, Code: table.CodeIdempotencyUnknown, Msg: err.Error()})
	case errors.Is(err, context.Canceled):
	default:
		log.Printf("appwrite: %v", err)
		h.fail(w, isGraphQL, &table.WriteError{Status: 500, Code: "internal", Msg: "internal error"})
	}
}

// fail writes an error. GraphQL errors use HTTP 200 once the request reached the schema, like
// the read endpoint, but authentication and authorization failures keep their status.
func (h *Handler) fail(w http.ResponseWriter, isGraphQL bool, we *table.WriteError) {
	w.Header().Set("Cache-Control", "no-store")
	if isGraphQL {
		status := we.Status
		if status >= 400 && status < 500 && status != 401 && status != 403 && status != 404 && status != 405 && status != 413 && status != 429 {
			status = http.StatusOK
		}
		writeJSON(w, status, gql.WriteFailure(we))
		return
	}
	writeJSON(w, we.Status, map[string]any{"error": we})
}

func reportedUser(r *http.Request) string {
	u := r.Header.Get(HeaderAppUser)
	if len(u) > 256 {
		u = u[:256]
	}
	return u
}

// location is the inserted row's URL when the table has a single-column primary key.
func location(r *http.Request, slug string, m table.Model, row table.Row) string {
	if len(m.PK) != 1 {
		return ""
	}
	var v any
	if json.Unmarshal(row[m.PK[0].Name], &v) != nil || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		b, _ := json.Marshal(v)
		s = string(b)
	}
	return "/modules/api/app/v1/" + slug + "/rows/" + url.PathEscape(s)
}

func writeRow(b *bytes.Buffer, m table.Model, row table.Row) {
	b.WriteByte('{')
	for i, f := range m.Fields {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.Name)
		b.Write(k)
		b.WriteByte(':')
		b.Write(row[f.Name])
	}
	b.WriteByte('}')
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	writeRaw(w, status, append(b, '\n'))
}

func writeRaw(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
