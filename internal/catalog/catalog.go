// Package catalog reads a dataset from booth-catalog, through booth-core's gateway, as the person
// asking (their own token and workspace): booth-api sees exactly the datasets that person can see,
// and never holds a standing catalog credential of its own (ADR 0007, ADR 0041).
//
// The postgres dataset shape is ADR 0102's (`format: "postgres"` plus a table block naming
// schema and table), which booth-catalog has not built yet. The JSON field that carries the table
// block is not final: ADR 0102 says only that it is its own field, not the Iceberg `table` block,
// and the coordinator records the name once booth-catalog reports it. Until then it is the one
// constant PostgresTableField below, so adopting the real name is a one-line change.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/projectbooth/booth-api/internal/apidef"
)

// PostgresTableField is the dataset JSON field holding a postgres dataset's {schema, name}.
// PROVISIONAL (see the package comment).
const PostgresTableField = "postgresTable"

// FormatPostgres is ADR 0102's discriminator value.
const FormatPostgres = "postgres"

var (
	ErrNotFound = errors.New("dataset not found in the catalog")
	// ErrDenied: the gateway or the catalog refused the caller's token for this request.
	ErrDenied = errors.New("the catalog refused this request")
)

// Dataset is the part of a catalog dataset booth-api uses.
type Dataset struct {
	ID     string
	Name   string
	Format string
	// Table is set only for a postgres dataset.
	Table  *apidef.TableRef
	Schema []apidef.CatalogColumn
}

// Client calls booth-catalog through core's gateway.
type Client struct {
	// CoreURL is booth-core's base URL; the catalog is at CoreURL/modules/catalog/.
	CoreURL string
	HTTP    *http.Client
}

const maxBody = 4 << 20

// Dataset fetches one dataset in workspace as the caller identified by token.
func (c *Client) Dataset(ctx context.Context, token, workspace, id string) (Dataset, error) {
	if c.CoreURL == "" {
		return Dataset{}, errors.New("BOOTH_CORE_URL is not configured, so the catalog can't be reached")
	}
	u := strings.TrimRight(c.CoreURL, "/") + "/modules/catalog/api/datasets/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Dataset{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace", workspace)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Dataset{}, fmt.Errorf("calling the catalog: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Dataset{}, fmt.Errorf("reading the catalog's response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Dataset{}, ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Dataset{}, ErrDenied
	case resp.StatusCode != http.StatusOK:
		return Dataset{}, fmt.Errorf("the catalog answered %d", resp.StatusCode)
	}
	return decode(body)
}

func decode(body []byte) (Dataset, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return Dataset{}, fmt.Errorf("decoding the catalog's dataset: %w", err)
	}
	var d Dataset
	var schema []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	for field, dst := range map[string]any{"id": &d.ID, "name": &d.Name, "format": &d.Format, "schema": &schema} {
		if v, ok := raw[field]; ok {
			if err := json.Unmarshal(v, dst); err != nil {
				return Dataset{}, fmt.Errorf("decoding the catalog's dataset field %q: %w", field, err)
			}
		}
	}
	if d.Format == "" {
		d.Format = "file" // booth-catalog's own default (ADR 0085)
	}
	for _, c := range schema {
		d.Schema = append(d.Schema, apidef.CatalogColumn{Name: c.Name, Description: c.Description})
	}
	if d.Format == FormatPostgres {
		var t apidef.TableRef
		if v, ok := raw[PostgresTableField]; ok {
			if err := json.Unmarshal(v, &t); err != nil {
				return Dataset{}, fmt.Errorf("decoding the catalog's %s: %w", PostgresTableField, err)
			}
		}
		if t.Schema == "" || t.Name == "" {
			return Dataset{}, fmt.Errorf("the catalog's postgres dataset %s has no %s {schema, name}", d.ID, PostgresTableField)
		}
		d.Table = &t
	}
	return d, nil
}
