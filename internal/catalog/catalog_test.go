package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/projectbooth/booth-api/internal/apidef"
)

func TestDataset(t *testing.T) {
	var gotPath, gotAuth, gotWS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotWS = r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("X-Workspace")
		switch r.URL.Path {
		case "/modules/catalog/api/datasets/pg":
			_, _ = w.Write([]byte(`{"id":"pg","name":"Orders","format":"postgres","postgresTable":{"schema":"public","name":"orders"},
				"schema":[{"name":"id","type":"bigint","description":"order id"}],"location":null,"tags":[]}`))
		case "/modules/catalog/api/datasets/file":
			_, _ = w.Write([]byte(`{"id":"file","name":"Raw","location":{"backendId":"b","path":"x"},"schema":[]}`))
		case "/modules/catalog/api/datasets/broken":
			_, _ = w.Write([]byte(`{"id":"broken","name":"B","format":"postgres"}`))
		case "/modules/catalog/api/datasets/denied":
			w.WriteHeader(http.StatusForbidden)
		case "/modules/catalog/api/datasets/boom":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{CoreURL: srv.URL + "/"}
	ctx := context.Background()

	d, err := c.Dataset(ctx, "tok", "acme", "pg")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" || gotWS != "acme" {
		t.Errorf("sent Authorization=%q X-Workspace=%q; the call must be made as the caller, through the gateway", gotAuth, gotWS)
	}
	if d.Format != FormatPostgres || d.Table == nil || *d.Table != (apidef.TableRef{Schema: "public", Name: "orders"}) {
		t.Errorf("dataset = %+v", d)
	}
	if len(d.Schema) != 1 || d.Schema[0].Description != "order id" {
		t.Errorf("schema = %+v", d.Schema)
	}

	if d, err := c.Dataset(ctx, "tok", "acme", "file"); err != nil || d.Format != "file" || d.Table != nil {
		t.Errorf("a pre-ADR-0085 dataset with no format: %+v, %v", d, err)
	}
	if _, err := c.Dataset(ctx, "tok", "acme", "broken"); err == nil {
		t.Error("a postgres dataset without a table block was accepted")
	}
	if _, err := c.Dataset(ctx, "tok", "acme", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := c.Dataset(ctx, "tok", "acme", "denied"); !errors.Is(err, ErrDenied) {
		t.Errorf("denied: %v", err)
	}
	if _, err := c.Dataset(ctx, "tok", "acme", "boom"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("502: %v", err)
	}
	if _, err := c.Dataset(ctx, "tok", "acme", "a/../b"); err != nil && gotPath != "/modules/catalog/api/datasets/a%2F..%2Fb" {
		t.Errorf("id not escaped into one path segment: %s", gotPath)
	}
	if _, err := (&Client{}).Dataset(ctx, "tok", "acme", "pg"); err == nil {
		t.Error("no core URL configured, but no error")
	}
}
