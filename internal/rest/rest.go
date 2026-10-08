// Package rest serves a generated API's REST endpoint (ADR 0100): list and get over one dataset's
// table, with filtering, keyset pagination and an OpenAPI 3.1 document, all read-only. It shares
// internal/table with the GraphQL endpoint, so a filter means the same SQL in both, the limits are
// the same, and cursors are interchangeable.
//
// Routes, relative to wherever the API is mounted:
//
//	GET /rows          ?limit=50&after=<cursor>&fields=a,b&order=-a,b&filter[a][gt]=1&filter[b]=x
//	GET /rows/{key}    ?fields=a,b      (only when the table has a single-column primary key)
//	GET /openapi.json
//
// Filters are AND-ed; `filter[f]=v` is `filter[f][eq]=v`; `in` takes the parameter repeated
// (`filter[f][in]=a&filter[f][in]=b`) so values may contain commas. or/not exist in GraphQL only.
// Filter parameters are namespaced under filter[...] so a column named `limit` or `order` can't
// collide with the paging parameters. Any other parameter is a 400, not ignored: a typo must not
// silently widen a result.
//
// Not mounted yet: generated endpoints go on core's public routes (ADR 0101), behind key
// verification and the dataset scope check, with the workspace's sidecar pool as the DB (ADR 0103).
package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/projectbooth/booth-api/internal/table"
)

// Handler serves one generated API.
type Handler struct {
	Model  table.Model
	Limits table.Limits
	DB     table.DB
	// OnInternalError, if set, receives errors the caller only sees as "internal error".
	OnInternalError func(error)
}

var reFilter = regexp.MustCompile(`^filter\[([^\[\]]+)\](?:\[([A-Za-z]+)\])?$`)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "this API is read-only")
		return
	}
	// Route on the escaped path, so a key containing an encoded '/' stays one segment.
	switch p := strings.TrimSuffix(r.URL.EscapedPath(), "/"); {
	case p == "/openapi.json":
		writeJSON(w, http.StatusOK, OpenAPI(h.Model, h.Limits))
	case p == "/rows":
		h.list(w, r)
	case strings.HasPrefix(p, "/rows/") && !strings.Contains(p[len("/rows/"):], "/"):
		key, err := url.PathUnescape(p[len("/rows/"):])
		if err != nil {
			writeError(w, http.StatusBadRequest, "malformed key")
			return
		}
		h.get(w, r, key)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	page, err := h.parseList(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	var res table.Result
	err = table.WithTx(r.Context(), h.DB, h.Limits.StatementTimeout, func(tx pgx.Tx) error {
		var err error
		res, err = table.Fetch(r.Context(), tx, h.Model, page, h.Limits)
		return err
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	var b bytes.Buffer
	b.WriteString(`{"data":[`)
	for i, row := range res.Rows {
		if i > 0 {
			b.WriteByte(',')
		}
		writeRow(&b, page.Fields, row)
	}
	b.WriteString(`],"page":`)
	pg, _ := json.Marshal(struct {
		HasNext    bool    `json:"hasNext"`
		NextCursor *string `json:"nextCursor"`
	}{res.HasNext, nextCursor(res)})
	b.Write(pg)
	b.WriteString("}\n")
	writeRaw(w, http.StatusOK, b.Bytes())
}

// nextCursor is only offered when there is a next page (GraphQL's endCursor is offered on every
// non-empty page, per the connection convention; REST clients just follow nextCursor until null).
func nextCursor(res table.Result) *string {
	if !res.HasNext {
		return nil
	}
	return res.EndCursor
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, key string) {
	if len(h.Model.PK) != 1 {
		writeError(w, http.StatusNotFound, "this table has no single-column primary key, so rows can't be fetched by key; use /rows with a filter")
		return
	}
	q := r.URL.Query()
	for k := range q {
		if k != "fields" {
			writeError(w, http.StatusBadRequest, "unknown parameter "+strconv.Quote(k))
			return
		}
	}
	fields, err := h.fields(q)
	if err != nil {
		h.fail(w, err)
		return
	}
	var row table.Row
	var found bool
	err = table.WithTx(r.Context(), h.DB, h.Limits.StatementTimeout, func(tx pgx.Tx) error {
		var err error
		row, found, err = table.Get(r.Context(), tx, h.Model, key, fields)
		return err
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no row with that key")
		return
	}
	var b bytes.Buffer
	b.WriteString(`{"data":`)
	writeRow(&b, fields, row)
	b.WriteString("}\n")
	writeRaw(w, http.StatusOK, b.Bytes())
}

func (h *Handler) parseList(r *http.Request) (table.Page, error) {
	q := r.URL.Query()
	page := table.Page{First: h.Limits.DefaultFirst}
	where := map[string]any{}
	for k, vals := range q {
		switch k {
		case "limit":
			n, err := strconv.Atoi(single(vals))
			if err != nil || len(vals) != 1 {
				return page, table.Errorf("limit must be one integer")
			}
			page.First = n
		case "after":
			if len(vals) != 1 {
				return page, table.Errorf("after must be given once")
			}
			page.After = vals[0]
		case "fields":
			// handled below
		case "order":
			if len(vals) != 1 {
				return page, table.Errorf("order must be given once, comma-separated")
			}
			for _, part := range strings.Split(vals[0], ",") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				desc := strings.HasPrefix(part, "-")
				page.OrderBy = append(page.OrderBy, table.OrderBy{Field: strings.TrimPrefix(part, "-"), Desc: desc})
			}
		default:
			m := reFilter.FindStringSubmatch(k)
			if m == nil {
				return page, table.Errorf("unknown parameter %q (filters are filter[field][op]=value)", k)
			}
			field, op := m[1], m[2]
			if op == "" {
				op = "eq"
			}
			if _, ok := h.Model.ByName[field]; !ok {
				return page, table.Errorf("no field %q", field)
			}
			ops, _ := where[field].(map[string]any)
			if ops == nil {
				ops = map[string]any{}
				where[field] = ops
			}
			if _, dup := ops[op]; dup {
				return page, table.Errorf("filter[%s][%s] given twice", field, op)
			}
			if op == "in" {
				list := make([]any, len(vals))
				for i, v := range vals {
					list[i] = v
				}
				ops[op] = list
				continue
			}
			if len(vals) != 1 {
				return page, table.Errorf("filter[%s][%s] must be given once (only in takes several values)", field, op)
			}
			ops[op] = vals[0]
		}
	}
	if len(where) > 0 {
		page.Where = where
	}
	fields, err := h.fields(q)
	if err != nil {
		return page, err
	}
	page.Fields = fields
	return page, nil
}

// fields is the `fields` parameter, or every field in column order.
func (h *Handler) fields(q map[string][]string) ([]string, error) {
	vals, ok := q["fields"]
	if !ok {
		out := make([]string, len(h.Model.Fields))
		for i, f := range h.Model.Fields {
			out[i] = f.Name
		}
		return out, nil
	}
	if len(vals) != 1 {
		return nil, table.Errorf("fields must be given once, comma-separated")
	}
	var out []string
	for _, n := range strings.Split(vals[0], ",") {
		if n = strings.TrimSpace(n); n != "" {
			if _, ok := h.Model.ByName[n]; !ok {
				return nil, table.Errorf("no field %q", n)
			}
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, table.Errorf("fields is empty")
	}
	return out, nil
}

func single(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// writeRow writes a row as a JSON object with keys in the requested order.
func writeRow(b *bytes.Buffer, fields []string, row table.Row) {
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f)
		b.Write(k)
		b.WriteByte(':')
		b.Write(row[f])
	}
	b.WriteByte('}')
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	var te *table.Error
	switch {
	case errors.As(err, &te) && te.Timeout:
		writeError(w, http.StatusServiceUnavailable, te.Msg)
	case errors.As(err, &te):
		writeError(w, http.StatusBadRequest, te.Msg)
	case errors.Is(err, context.Canceled):
		// client went away
	default:
		if h.OnInternalError != nil {
			h.OnInternalError(err)
		}
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	writeRaw(w, status, append(b, '\n'))
}

func writeRaw(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store") // per-key data; nothing in between should keep it
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
