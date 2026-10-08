// Package apidef is a generated API's definition: the snapshot of one catalog dataset's table,
// taken when a user generates the API (docs/design-v0.md §3, "snapshot, not live"). REST and
// GraphQL are both built from this stored snapshot, never from a live look at the table, so a
// client's schema doesn't change under it; regenerating is an explicit user action.
//
// Pure functions only. Fetching the dataset (internal/catalog), introspecting the table
// (internal/source) and storing the result (internal/store) live elsewhere.
package apidef

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TableRef names the table a dataset lives in, inside its workspace's booth-database database
// (ADR 0102: the database is implied by the workspace, so it is never stored).
type TableRef struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

// Column is one column of the snapshot. Type is PostgreSQL's own rendering (format_type), which is
// authoritative; Description comes from the catalog where the names match (ADR 0102 item 4: the
// catalog schema is descriptive only).
type Column struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Nullable    bool   `json:"nullable"`
	Description string `json:"description,omitempty"`
}

// Definition is one generated API. There is at most one per (workspace, dataset).
type Definition struct {
	ID          string   `json:"id"`
	Workspace   string   `json:"-"`
	DatasetID   string   `json:"datasetId"`
	DatasetName string   `json:"datasetName"`
	Slug        string   `json:"slug"`
	Table       TableRef `json:"table"`
	Columns     []Column `json:"columns"`
	// PrimaryKey lists the primary key's columns in key order; empty when the table has none.
	PrimaryKey  []string  `json:"primaryKey"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
	GeneratedAt time.Time `json:"generatedAt"`
}

// CatalogColumn is a column as the catalog describes it (free-form type, ignored here).
type CatalogColumn struct {
	Name        string
	Description string
}

// Mismatch is one disagreement between the catalog's schema and the real table.
type Mismatch struct {
	Column  string `json:"column"`
	Problem string `json:"problem"`
}

const (
	ProblemNotInTable   = "in the catalog but not in the table"
	ProblemNotInCatalog = "in the table but not in the catalog"
)

// Reconcile compares the catalog's column list with the table's real columns and returns the
// columns with descriptions filled in, plus every mismatch. An empty catalog schema means the
// dataset was registered without one; that is allowed and yields no mismatches, since there is
// nothing for the API to contradict. A non-empty one must name exactly the table's columns, so the
// generated API never exposes columns the catalog doesn't describe or claims ones it lacks.
func Reconcile(catalog []CatalogColumn, table []Column) ([]Column, []Mismatch) {
	out := make([]Column, len(table))
	copy(out, table)
	if len(catalog) == 0 {
		return out, nil
	}
	desc := make(map[string]string, len(catalog))
	for _, c := range catalog {
		desc[c.Name] = c.Description
	}
	var mm []Mismatch
	inTable := make(map[string]bool, len(table))
	for i, c := range out {
		inTable[c.Name] = true
		d, ok := desc[c.Name]
		if !ok {
			mm = append(mm, Mismatch{Column: c.Name, Problem: ProblemNotInCatalog})
			continue
		}
		out[i].Description = d
	}
	for _, c := range catalog {
		if !inTable[c.Name] {
			mm = append(mm, Mismatch{Column: c.Name, Problem: ProblemNotInTable})
		}
	}
	sort.SliceStable(mm, func(i, j int) bool { return mm[i].Column < mm[j].Column })
	return out, mm
}

// MismatchError reports a catalog/table disagreement that blocks generation.
type MismatchError struct{ Mismatches []Mismatch }

func (e *MismatchError) Error() string {
	parts := make([]string, len(e.Mismatches))
	for i, m := range e.Mismatches {
		parts[i] = fmt.Sprintf("%s (%s)", m.Column, m.Problem)
	}
	return "the catalog's schema doesn't match the table: " + strings.Join(parts, ", ")
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// MaxSlug bounds a slug, which becomes a URL path segment of the generated endpoints.
const MaxSlug = 63

// Slug derives a URL-safe name from a dataset name: lowercase ASCII letters and digits separated
// by single hyphens. n > 1 appends "-n", for a slug already taken in the workspace.
func Slug(name string, n int) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "dataset"
	}
	suffix := ""
	if n > 1 {
		suffix = fmt.Sprintf("-%d", n)
	}
	if len(s) > MaxSlug-len(suffix) {
		s = strings.TrimRight(s[:MaxSlug-len(suffix)], "-")
	}
	return s + suffix
}
