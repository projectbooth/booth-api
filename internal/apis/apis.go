// Package apis is the dataset-to-API definition flow: a person picks a catalog dataset, booth-api
// fetches it from the catalog as that person, introspects the real table, reconciles the two and
// stores the snapshot (docs/design-v0.md §3; ADR 0102 for the dataset shape).
package apis

import (
	"context"
	"errors"
	"fmt"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/auth"
	"github.com/projectbooth/booth-api/internal/catalog"
	"github.com/projectbooth/booth-api/internal/gql"
	"github.com/projectbooth/booth-api/internal/rest"
	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/table"
)

// ErrUnsupportedFormat: v0 generates APIs for postgres datasets only (ARCHITECTURE.md item 53).
var ErrUnsupportedFormat = errors.New("only datasets of format \"postgres\" (a booth-database table) can have an API in v0")

// ErrNoColumns: the table has no columns at all, so there is nothing to serve.
var ErrNoColumns = errors.New("the table has no columns")

// Catalog is the slice of internal/catalog this package needs.
type Catalog interface {
	Dataset(ctx context.Context, token, workspace, id string) (catalog.Dataset, error)
}

// Store is the slice of internal/store this package needs.
type Store interface {
	CreateAPI(ctx context.Context, d apidef.Definition) (apidef.Definition, error)
	UpdateSnapshot(ctx context.Context, d apidef.Definition) (apidef.Definition, error)
	GetAPI(ctx context.Context, workspace, id string) (apidef.Definition, error)
	ListAPIs(ctx context.Context, workspace string) ([]apidef.Definition, error)
	DeleteAPI(ctx context.Context, workspace, id string) error
}

// Service generates, regenerates, lists and deletes APIs.
type Service struct {
	Catalog Catalog
	Store   Store
	Pools   source.Pools
}

// Generate creates the API for one dataset in the caller's workspace.
func (s Service) Generate(ctx context.Context, who auth.Identity, datasetID string) (apidef.Definition, error) {
	ds, err := s.Catalog.Dataset(ctx, who.Token, who.Workspace, datasetID)
	if err != nil {
		return apidef.Definition{}, err
	}
	d, err := s.snapshot(ctx, who.Workspace, ds)
	if err != nil {
		return apidef.Definition{}, err
	}
	d.CreatedBy = who.Subject
	return s.Store.CreateAPI(ctx, d)
}

// Regenerate re-reads the dataset and the table and replaces the snapshot, keeping the API's id,
// slug and keys. This is the only way a generated API's columns ever change.
func (s Service) Regenerate(ctx context.Context, who auth.Identity, id string) (apidef.Definition, error) {
	old, err := s.Store.GetAPI(ctx, who.Workspace, id)
	if err != nil {
		return apidef.Definition{}, err
	}
	ds, err := s.Catalog.Dataset(ctx, who.Token, who.Workspace, old.DatasetID)
	if err != nil {
		return apidef.Definition{}, err
	}
	d, err := s.snapshot(ctx, who.Workspace, ds)
	if err != nil {
		return apidef.Definition{}, err
	}
	d.ID = old.ID
	return s.Store.UpdateSnapshot(ctx, d)
}

func (s Service) snapshot(ctx context.Context, workspace string, ds catalog.Dataset) (apidef.Definition, error) {
	if ds.Format != catalog.FormatPostgres || ds.Table == nil {
		return apidef.Definition{}, ErrUnsupportedFormat
	}
	pool, err := s.Pools.Pool(ctx, workspace)
	if err != nil {
		return apidef.Definition{}, err
	}
	tbl, err := source.Introspect(ctx, pool, *ds.Table)
	if err != nil {
		return apidef.Definition{}, err
	}
	if len(tbl.Columns) == 0 {
		return apidef.Definition{}, ErrNoColumns
	}
	cols, mm := apidef.Reconcile(ds.Schema, tbl.Columns)
	if len(mm) > 0 {
		return apidef.Definition{}, &apidef.MismatchError{Mismatches: mm}
	}
	d := apidef.Definition{
		Workspace: workspace, DatasetID: ds.ID, DatasetName: ds.Name,
		Table: *ds.Table, Columns: cols, PrimaryKey: tbl.PrimaryKey,
	}
	// Refuse now rather than store an API that can never serve anything (every column of a type
	// the API omits, e.g. all bytea).
	if _, err := gql.New(table.NewModel(d), gql.DefaultLimits); err != nil {
		return apidef.Definition{}, err
	}
	return d, nil
}

// Schema is a generated API's GraphQL schema as the endpoint will serve it, plus the columns left
// out because their types have no faithful mapping (docs/design-v0.md §3).
type Schema struct {
	SDL     string          `json:"sdl"`
	Omitted []table.Omitted `json:"omitted"`
}

// GraphQLSchema renders the stored snapshot's schema.
func (s Service) GraphQLSchema(ctx context.Context, workspace, id string) (Schema, error) {
	d, err := s.Store.GetAPI(ctx, workspace, id)
	if err != nil {
		return Schema{}, err
	}
	m := table.NewModel(d)
	e, err := gql.New(m, gql.DefaultLimits)
	if err != nil {
		return Schema{}, err
	}
	omitted := m.Omitted
	if omitted == nil {
		omitted = []table.Omitted{}
	}
	return Schema{SDL: e.SDL, Omitted: omitted}, nil
}

// List returns the workspace's APIs.
func (s Service) List(ctx context.Context, workspace string) ([]apidef.Definition, error) {
	return s.Store.ListAPIs(ctx, workspace)
}

// Get returns one API.
func (s Service) Get(ctx context.Context, workspace, id string) (apidef.Definition, error) {
	return s.Store.GetAPI(ctx, workspace, id)
}

// Delete removes an API and drops it from every key's scope.
func (s Service) Delete(ctx context.Context, workspace, id string) error {
	if err := s.Store.DeleteAPI(ctx, workspace, id); err != nil {
		return fmt.Errorf("deleting API %s: %w", id, err)
	}
	return nil
}

// OpenAPI renders the stored snapshot's REST OpenAPI document.
func (s Service) OpenAPI(ctx context.Context, workspace, id string) (map[string]any, error) {
	d, err := s.Store.GetAPI(ctx, workspace, id)
	if err != nil {
		return nil, err
	}
	m := table.NewModel(d)
	if len(m.Fields) == 0 {
		return nil, gql.ErrNoFields
	}
	return rest.OpenAPI(m, table.DefaultLimits), nil
}
