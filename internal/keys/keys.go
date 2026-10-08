package keys

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/projectbooth/booth-api/internal/store"
)

const (
	maxName     = 100
	maxDatasets = 100
)

// ErrInvalid wraps a validation failure on a key request; its message is safe to show the user.
var ErrInvalid = errors.New("invalid key request")

// ErrUnauthenticated is the one error Verify returns for every refusal (malformed, unknown,
// wrong secret, revoked), so a caller can't tell which and probe for valid ids.
var ErrUnauthenticated = errors.New("invalid or revoked API key")

// Store is the slice of internal/store this package needs.
type Store interface {
	CreateKey(ctx context.Context, k store.Key) (store.Key, error)
	ListKeys(ctx context.Context, workspace string) ([]store.Key, error)
	KeyByID(ctx context.Context, id string) (store.Key, error)
	RevokeKey(ctx context.Context, workspace, id, by string) (store.Key, error)
}

// Service issues, lists, revokes and verifies keys.
type Service struct{ Store Store }

// Issued is a newly created key. Secret is the full presented key: returned exactly once, here,
// and never retrievable again.
type Issued struct {
	store.Key
	Secret string `json:"secret"`
}

// Issue creates a key in workspace, scoped to datasetIDs, owned by the issuing person (ADR 0103:
// createdBy becomes the owner on the workspace's workload token).
func (s Service) Issue(ctx context.Context, workspace, name string, datasetIDs []string, createdBy, createdByName string) (Issued, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxName {
		return Issued{}, fmt.Errorf("%w: name must be 1-%d characters", ErrInvalid, maxName)
	}
	ids := dedupe(datasetIDs)
	if len(ids) == 0 || len(ids) > maxDatasets {
		return Issued{}, fmt.Errorf("%w: a key must be scoped to 1-%d datasets", ErrInvalid, maxDatasets)
	}
	id, secret, hash, err := newKey()
	if err != nil {
		return Issued{}, err
	}
	k, err := s.Store.CreateKey(ctx, store.Key{
		ID: id, Workspace: workspace, Name: name, SecretHash: hash,
		CreatedBy: createdBy, CreatedByName: createdByName, DatasetIDs: ids,
	})
	if errors.Is(err, store.ErrUnknownDatasets) {
		return Issued{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err != nil {
		return Issued{}, err
	}
	// Audit line: who issued what, never the secret or its hash.
	log.Printf("audit: api key issued id=%s workspace=%s by=%s datasets=%s", id, workspace, createdBy, strings.Join(ids, ","))
	return Issued{Key: k, Secret: secret}, nil
}

// List returns the workspace's keys, without secrets (there are none to return).
func (s Service) List(ctx context.Context, workspace string) ([]store.Key, error) {
	return s.Store.ListKeys(ctx, workspace)
}

// Revoke revokes a key in workspace. It takes effect on the key's next use: Verify reads the row
// every time, with no cache.
func (s Service) Revoke(ctx context.Context, workspace, id, by string) (store.Key, error) {
	k, err := s.Store.RevokeKey(ctx, workspace, id, by)
	if err == nil {
		log.Printf("audit: api key revoked id=%s workspace=%s by=%s", id, workspace, by)
	}
	return k, err
}

// Principal is what a verified key grants: read access to these datasets in this workspace, on
// behalf of the person who issued it.
type Principal struct {
	KeyID      string
	Workspace  string
	DatasetIDs []string
	Owner      string
}

// Allows reports whether the key's scope includes datasetID.
func (p Principal) Allows(datasetID string) bool {
	i := sort.SearchStrings(p.DatasetIDs, datasetID)
	return i < len(p.DatasetIDs) && p.DatasetIDs[i] == datasetID
}

// Verify checks a presented key. Not yet wired to any route: generated endpoints are served on
// core's public routes (ADR 0101), which this module mounts once core's change has landed.
func (s Service) Verify(ctx context.Context, presented string) (Principal, error) {
	id, secret, err := parse(presented)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	k, err := s.Store.KeyByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	if !matches(secret, k.SecretHash) || k.RevokedAt != nil {
		return Principal{}, ErrUnauthenticated
	}
	scope := append([]string(nil), k.DatasetIDs...)
	sort.Strings(scope) // byte order for Allows, whatever the database's collation
	return Principal{KeyID: k.ID, Workspace: k.Workspace, DatasetIDs: scope, Owner: k.CreatedBy}, nil
}

// dedupe returns the non-empty ids, trimmed, sorted and unique.
func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
