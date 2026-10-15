// Package writes is booth-api's write model (ADR 0117 points 1, 3, 4 and "Reconciled design";
// docs/decisions/0008): an owner's write settings per API, write grants to apps, and the app write
// path itself.
//
// Layers, each refusing to widen what the one above allows:
//
//  1. booth-api: the API's settings (operations, writable columns) narrowed by the caller's live
//     grant; every row validated against the stored snapshot; one row per request.
//  2. booth-core: the write sidecar's token is `apiwrite:<api-id>`, minted for the granting owner
//     at roleCeiling editor. Core refuses the mint if that owner has lost access or not signed in
//     for 7 days, and its broker lets the token ask for a `tablewrite` lease only.
//  3. booth-database: the lease is a member of one per-grant write group holding SELECT, the
//     declared operations, column-level INSERT/UPDATE and sequence USAGE on the one table; never
//     `_rw`, never DDL.
package writes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/appauth"
	"github.com/projectbooth/booth-api/internal/auth"
	"github.com/projectbooth/booth-api/internal/broker"
	"github.com/projectbooth/booth-api/internal/sidecars"
	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/store"
	"github.com/projectbooth/booth-api/internal/table"
	"github.com/projectbooth/booth-api/internal/workload"
)

// Defaults (ADR 0117 "Reconciled design" item 5).
const (
	DefaultRate           = 10
	DefaultBurst          = 50
	DefaultAuditRetention = 90 * 24 * time.Hour
	DefaultIdempotencyTTL = 24 * time.Hour
	// MaxBody is the largest write request: one row.
	MaxBody = 64 << 10
	// MaxIdempotencyKey bounds an Idempotency-Key.
	MaxIdempotencyKey = 255
)

var (
	// ErrDataAccessOff: no minting credential, so no write lease can exist.
	ErrDataAccessOff = errors.New("writing workspace data is not available: booth-api has no workload minting credential (core.url unset)")
	// ErrRegenerate: the API's snapshot predates write support.
	ErrRegenerate = errors.New("this API was generated before writes existed; regenerate it, then turn writes on")
	// ErrWriteKeysNotBuilt: a key:<id> principal (ADR 0117: apps first).
	ErrWriteKeysNotBuilt = errors.New("write grants to API keys aren't available yet; only apps (streamlit:<workspace>:<appId>) can be granted writes")
	// ErrBadPrincipal: not an app subject in this workspace.
	ErrBadPrincipal = errors.New("principal must be an app's token subject in this workspace: streamlit:<workspace>:<appId>")
	// ErrDeleteWritable: an editor tried to delete an API an owner made writable.
	ErrDeleteWritable = errors.New("writes are on for this API; an owner must turn them off before it can be deleted")
)

// SettingsError is a refused settings or grant change, safe to show the owner.
type SettingsError struct {
	Msg      string
	Problems []table.FieldProblem
}

func (e *SettingsError) Error() string { return e.Msg }

// Store is the slice of internal/store this package needs.
type Store interface {
	GetAPI(ctx context.Context, workspace, id string) (apidef.Definition, error)
	GetAPIBySlug(ctx context.Context, workspace, slug string) (apidef.Definition, error)
	SetWriteSettings(ctx context.Context, workspace, apiID string, ops, cols []string, by string) (apidef.WriteSettings, error)
	DeleteWriteSettings(ctx context.Context, workspace, apiID, by string) ([]store.Grant, bool, error)
	CreateGrant(ctx context.Context, g store.Grant) (store.Grant, *store.Grant, error)
	ListGrants(ctx context.Context, workspace string, f store.GrantFilter) ([]store.Grant, error)
	LiveGrant(ctx context.Context, workspace, apiID, principal string) (store.Grant, error)
	RevokeGrant(ctx context.Context, workspace, id, by string) (store.Grant, error)
	RevokePrincipal(ctx context.Context, workspace, principal, by string) ([]store.Grant, error)
	HasLiveGrantsBy(ctx context.Context, apiID, owner string) (bool, error)
	ClaimIdempotencyKey(ctx context.Context, apiID, principal, key, hash string, ttl time.Duration) (store.Idempotent, bool, error)
	FinishIdempotencyKey(ctx context.Context, apiID, principal, key string, response json.RawMessage) error
	ReleaseIdempotencyKey(ctx context.Context, apiID, principal, key string) error
	LogWrite(ctx context.Context, e store.WriteLogEntry) error
	LogAdmin(ctx context.Context, workspace, apiID, actor, action string, detail any) error
	WriteLog(ctx context.Context, workspace, apiID string, limit int) ([]store.WriteLogRecord, error)
}

// Pools runs write sidecars (internal/sidecars.Manager).
type Pools interface {
	WritePool(ctx context.Context, w sidecars.WriteSpec) (*pgxpool.Pool, error)
	StopWrites(apiID, owner, why string) int
}

// Minter mints workload tokens (internal/workload.Minter).
type Minter interface {
	Mint(ctx context.Context, r workload.Request) (workload.Token, error)
}

// Broker makes booth-api's own broker calls (internal/broker.Client).
type Broker interface {
	TableWrite(ctx context.Context, token, workspace string, scope json.RawMessage, onBehalfOf string) error
	RevokeTableWrite(ctx context.Context, token, workspace, grant, onBehalfOf string) error
}

// Limiter is the per-principal rate limit (internal/ratelimit).
type Limiter interface {
	Allow(key string) (bool, time.Duration)
}

// Service is the write model. Pools, Minter and Broker are nil when data access is off.
type Service struct {
	Store   Store
	Pools   Pools
	Minter  Minter
	Broker  Broker
	Limiter Limiter
	// Timeout is a write's statement timeout. IdempotencyTTL is how long a key is remembered.
	Timeout        time.Duration
	IdempotencyTTL time.Duration
	Now            func() time.Time
}

func (s Service) dataAccess() bool { return s.Pools != nil && s.Minter != nil && s.Broker != nil }

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// GrantID is the booth-database grant id for an API's write group. The `api:` prefix lets the
// Database module's Tables view name the API in its "Writable through API" badge.
func GrantID(apiID string) string { return "api:" + apiID }

// Scope is the broker scope for an API's current settings (contracts/credential-broker.md,
// `postgres` `tablewrite`).
func Scope(def apidef.Definition) json.RawMessage {
	ops := canonicalOps(def.Writes.Operations)
	cols := def.Writes.Columns
	if !slices.Contains(ops, apidef.OpInsert) && !slices.Contains(ops, apidef.OpUpdate) {
		cols = []string{} // delete only: booth-database refuses columns
	}
	if cols == nil {
		cols = []string{}
	}
	b, _ := json.Marshal(map[string]any{
		"workspace": def.Workspace,
		"write": map[string]any{
			"grant": GrantID(def.ID),
			"tables": []any{map[string]any{
				"schema": def.Table.Schema, "name": def.Table.Name, "operations": ops, "columns": cols,
			}},
		},
	})
	return b
}

func canonicalOps(ops []string) []string {
	out := []string{}
	for _, o := range apidef.Operations {
		if slices.Contains(ops, o) {
			out = append(out, o)
		}
	}
	return out
}

// ---- settings ----------------------------------------------------------------------------

// ColumnView is one column as the Writes panel shows it.
type ColumnView struct {
	Column   string `json:"column"`
	Field    string `json:"field,omitempty"`
	Type     string `json:"type"`
	Writable bool   `json:"writable"`
	// Reason says why a column can't be made writable.
	Reason string `json:"reason,omitempty"`
	// Required: an insert must set it (NOT NULL, no default).
	Required bool `json:"required"`
}

// View is an API's write settings with what the panel needs to change them.
type View struct {
	APIID           string                `json:"apiId"`
	Writes          *apidef.WriteSettings `json:"writes"`
	Columns         []ColumnView          `json:"columns"`
	HasPrimaryKey   bool                  `json:"hasPrimaryKey"`
	NeedsRegenerate bool                  `json:"needsRegenerate"`
	DataAccess      bool                  `json:"dataAccess"`
}

// Settings returns an API's write settings view.
func (s Service) Settings(ctx context.Context, workspace, apiID string) (View, error) {
	def, err := s.Store.GetAPI(ctx, workspace, apiID)
	if err != nil {
		return View{}, err
	}
	return s.view(def), nil
}

func (s Service) view(def apidef.Definition) View {
	m := table.NewModel(def)
	required := map[string]bool{}
	for _, c := range m.RequiredOnInsert() {
		required[c] = true
	}
	v := View{APIID: def.ID, Writes: def.Writes, HasPrimaryKey: len(m.PK) > 0,
		NeedsRegenerate: def.SnapshotVersion < apidef.SnapshotVersion, DataAccess: s.dataAccess(), Columns: []ColumnView{}}
	for _, c := range def.Columns {
		cv := ColumnView{Column: c.Name, Type: c.Type, Required: required[c.Name]}
		if f, ok := m.FieldForColumn(c.Name); ok {
			cv.Field = f.Name
			cv.Writable, cv.Reason = m.WritableCandidate(f)
		} else {
			cv.Reason = "its type isn't served by the API"
		}
		v.Columns = append(v.Columns, cv)
	}
	return v
}

// validateSettings checks ops and cols against the snapshot: returns them deduplicated, in
// canonical/column order.
func validateSettings(def apidef.Definition, ops, cols []string) ([]string, []string, error) {
	if def.SnapshotVersion < apidef.SnapshotVersion {
		return nil, nil, ErrRegenerate
	}
	m := table.NewModel(def)
	var problems []table.FieldProblem
	for _, o := range ops {
		if !slices.Contains(apidef.Operations, o) {
			return nil, nil, &SettingsError{Msg: fmt.Sprintf("unknown operation %q (insert, update, delete)", o)}
		}
	}
	ops = canonicalOps(ops)
	if len(ops) == 0 {
		return nil, nil, &SettingsError{Msg: "turn on at least one operation, or turn writes off"}
	}
	if (slices.Contains(ops, apidef.OpUpdate) || slices.Contains(ops, apidef.OpDelete)) && len(m.PK) == 0 {
		return nil, nil, &SettingsError{Msg: "update and delete need a primary key, and this table has none (or its key isn't served by the API); turn on insert only"}
	}
	wantCols := map[string]bool{}
	for _, c := range cols {
		f, ok := m.FieldForColumn(c)
		if !ok {
			problems = append(problems, table.FieldProblem{Field: c, Problem: "not a column this API serves"})
			continue
		}
		if ok, why := m.WritableCandidate(f); !ok {
			problems = append(problems, table.FieldProblem{Field: c, Problem: "can't be writable: " + why})
			continue
		}
		wantCols[c] = true
	}
	ordered := []string{}
	for _, c := range def.Columns {
		if wantCols[c.Name] {
			ordered = append(ordered, c.Name)
		}
	}
	writesValues := slices.Contains(ops, apidef.OpInsert) || slices.Contains(ops, apidef.OpUpdate)
	switch {
	case writesValues && len(ordered) == 0 && len(problems) == 0:
		return nil, nil, &SettingsError{Msg: "insert and update need at least one writable column"}
	case !writesValues && len(cols) > 0:
		return nil, nil, &SettingsError{Msg: "delete alone writes no columns; leave the column list empty"}
	}
	if slices.Contains(ops, apidef.OpInsert) {
		for _, c := range m.RequiredOnInsert() {
			if !wantCols[c] {
				problems = append(problems, table.FieldProblem{Field: c, Problem: "required on insert (NOT NULL, no default), so it must be writable"})
			}
		}
	}
	if len(problems) > 0 {
		return nil, nil, &SettingsError{Msg: "these columns can't be saved as listed", Problems: problems}
	}
	return ops, ordered, nil
}

// SaveSettings turns writes on, or changes them, for an API (owners only; the HTTP layer checks
// the role). The full scope goes to the broker first, as the saving owner: booth-database reissues
// the API's write group to exactly these settings (or refuses them, which refuses the save), and
// only then are the settings stored and the API's write sidecars restarted.
func (s Service) SaveSettings(ctx context.Context, who auth.Identity, apiID string, ops, cols []string) (View, error) {
	if !s.dataAccess() {
		return View{}, ErrDataAccessOff
	}
	def, err := s.Store.GetAPI(ctx, who.Workspace, apiID)
	if err != nil {
		return View{}, err
	}
	ops, cols, err = validateSettings(def, ops, cols)
	if err != nil {
		return View{}, err
	}
	next := def
	next.Writes = &apidef.WriteSettings{Operations: ops, Columns: cols}
	if err := s.reissue(ctx, next, who.Subject, who.Subject); err != nil {
		return View{}, err
	}
	w, err := s.Store.SetWriteSettings(ctx, who.Workspace, apiID, ops, cols, who.Subject)
	if err != nil {
		return View{}, err
	}
	def.Writes = &w
	s.Pools.StopWrites(apiID, "", "write settings saved")
	s.audit(ctx, who.Workspace, apiID, who.Subject, "writes.save", map[string]any{"operations": ops, "columns": cols, "version": w.Version})
	return s.view(def), nil
}

// DisableResult says what turning writes off did.
type DisableResult struct {
	RevokedGrants   int    `json:"revokedGrants"`
	DatabaseRevoked bool   `json:"databaseRevoked"`
	DatabaseError   string `json:"databaseError,omitempty"`
}

// DisableWrites turns writes off for an API: settings removed and every grant revoked (one
// transaction, so writes stop at once), write sidecars stopped, then the revoke request ends the
// write group's leases in the database. A failed revoke request is reported, not fatal: nothing in
// booth-api can use the group any more, and booth-database's reaper drops an empty group.
func (s Service) DisableWrites(ctx context.Context, who auth.Identity, apiID string) (DisableResult, error) {
	revoked, existed, err := s.Store.DeleteWriteSettings(ctx, who.Workspace, apiID, who.Subject)
	if err != nil {
		return DisableResult{}, err
	}
	res := DisableResult{RevokedGrants: len(revoked)}
	if !existed {
		return res, nil
	}
	if s.dataAccess() {
		s.Pools.StopWrites(apiID, "", "writes turned off")
		if err := s.revoke(ctx, who.Workspace, apiID, who.Subject); err != nil {
			log.Printf("writes: revoke request for API %s in workspace %s: %v", apiID, who.Workspace, err)
			res.DatabaseError = err.Error()
		} else {
			res.DatabaseRevoked = true
		}
	}
	s.audit(ctx, who.Workspace, apiID, who.Subject, "writes.off", res)
	return res, nil
}

// BeforeDeleteAPI turns writes off before an API is deleted. Only an owner may delete a writable
// API (writes are an owner's decision); for anyone else it refuses.
func (s Service) BeforeDeleteAPI(ctx context.Context, who auth.Identity, apiID string) error {
	def, err := s.Store.GetAPI(ctx, who.Workspace, apiID)
	if err != nil || def.Writes == nil {
		return err
	}
	if who.Role != auth.RoleOwner {
		return ErrDeleteWritable
	}
	_, err = s.DisableWrites(ctx, who, apiID)
	return err
}

// reissue sends the full scope for def's settings, minted for owner, onBehalfOf for audit.
func (s Service) reissue(ctx context.Context, def apidef.Definition, owner, onBehalfOf string) error {
	tok, err := s.Minter.Mint(ctx, workload.WriteRequest(def.Workspace, def.ID, owner))
	if err != nil {
		return err
	}
	err = s.Broker.TableWrite(ctx, tok.JWT, def.Workspace, Scope(def), onBehalfOf)
	var se *broker.ScopeError
	if errors.As(err, &se) {
		return &SettingsError{Msg: se.Error()}
	}
	return err
}

func (s Service) revoke(ctx context.Context, workspace, apiID, owner string) error {
	tok, err := s.Minter.Mint(ctx, workload.WriteRequest(workspace, apiID, owner))
	if err != nil {
		return err
	}
	return s.Broker.RevokeTableWrite(ctx, tok.JWT, workspace, GrantID(apiID), owner)
}

func (s Service) audit(ctx context.Context, workspace, apiID, actor, action string, detail any) {
	if err := s.Store.LogAdmin(ctx, workspace, apiID, actor, action, detail); err != nil {
		log.Printf("writes: recording %s for API %s: %v", action, apiID, err)
	}
	log.Printf("writes: %s api=%s workspace=%s by=%s", action, apiID, workspace, actor)
}

// ---- grants ------------------------------------------------------------------------------

// Grant gives an app write access to an API (owners only). ops defaults to insert; ops and cols
// can only narrow the API's settings (cols nil follows them). An existing grant for the same app
// and API is replaced.
func (s Service) Grant(ctx context.Context, who auth.Identity, apiID, principal string, ops, cols []string) (store.Grant, error) {
	if strings.HasPrefix(principal, "key:") {
		return store.Grant{}, ErrWriteKeysNotBuilt
	}
	if !appauth.ValidPrincipal(principal, who.Workspace) {
		return store.Grant{}, ErrBadPrincipal
	}
	def, err := s.Store.GetAPI(ctx, who.Workspace, apiID)
	if err != nil {
		return store.Grant{}, err
	}
	if def.Writes == nil {
		return store.Grant{}, store.ErrWritesOff
	}
	if len(ops) == 0 {
		ops = []string{apidef.OpInsert}
	}
	for _, o := range ops {
		if !def.Writes.Allows(o) {
			return store.Grant{}, &SettingsError{Msg: fmt.Sprintf("the API doesn't allow %q; a grant can narrow the API's operations, never widen them", o)}
		}
	}
	if cols != nil {
		for _, c := range cols {
			if !slices.Contains(def.Writes.Columns, c) {
				return store.Grant{}, &SettingsError{Msg: fmt.Sprintf("%q isn't writable in the API's settings; a grant can narrow the writable columns, never widen them", c)}
			}
		}
		cols = slices.Compact(slices.Sorted(slices.Values(cols)))
	}
	g, replaced, err := s.Store.CreateGrant(ctx, store.Grant{
		Workspace: who.Workspace, APIID: apiID, Principal: principal, Operations: canonicalOps(ops), Columns: cols,
		GrantedBy: who.Subject, GrantedByName: who.DisplayName,
	})
	if err != nil {
		return store.Grant{}, err
	}
	if replaced != nil && replaced.GrantedBy != g.GrantedBy {
		s.stopIfLast(ctx, replaced.APIID, replaced.GrantedBy)
	}
	s.audit(ctx, who.Workspace, apiID, who.Subject, "grant.create", map[string]any{"grant": g.ID, "principal": principal, "operations": g.Operations, "columns": g.Columns})
	return g, nil
}

// Grants lists the workspace's grants.
func (s Service) Grants(ctx context.Context, workspace string, f store.GrantFilter) ([]store.Grant, error) {
	return s.Store.ListGrants(ctx, workspace, f)
}

// RevokeGrant revokes one grant; the granting owner's write sidecar for the API stops if that was
// their last grant on it.
func (s Service) RevokeGrant(ctx context.Context, who auth.Identity, id string) (store.Grant, error) {
	g, err := s.Store.RevokeGrant(ctx, who.Workspace, id, who.Subject)
	if err != nil {
		return g, err
	}
	s.stopIfLast(ctx, g.APIID, g.GrantedBy)
	s.audit(ctx, who.Workspace, g.APIID, who.Subject, "grant.revoke", map[string]any{"grant": g.ID, "principal": g.Principal})
	return g, nil
}

// RevokePrincipal revokes every live grant an app holds in the workspace, in one statement
// (booth-streamlit calls this on take-ownership), and stops the write sidecars nobody needs now.
func (s Service) RevokePrincipal(ctx context.Context, who auth.Identity, principal string) ([]store.Grant, error) {
	if strings.HasPrefix(principal, "key:") {
		return nil, ErrWriteKeysNotBuilt
	}
	if !appauth.ValidPrincipal(principal, who.Workspace) {
		return nil, ErrBadPrincipal
	}
	gs, err := s.Store.RevokePrincipal(ctx, who.Workspace, principal, who.Subject)
	if err != nil {
		return nil, err
	}
	seen := map[[2]string]bool{}
	for _, g := range gs {
		k := [2]string{g.APIID, g.GrantedBy}
		if !seen[k] {
			seen[k] = true
			s.stopIfLast(ctx, g.APIID, g.GrantedBy)
		}
	}
	s.audit(ctx, who.Workspace, "", who.Subject, "grant.revoke-principal", map[string]any{"principal": principal, "revoked": len(gs)})
	return gs, nil
}

func (s Service) stopIfLast(ctx context.Context, apiID, owner string) {
	if s.Pools == nil {
		return
	}
	live, err := s.Store.HasLiveGrantsBy(ctx, apiID, owner)
	if err != nil {
		log.Printf("writes: checking %s's remaining grants on API %s: %v; stopping their write sidecar anyway", owner, apiID, err)
	}
	if err != nil || !live {
		s.Pools.StopWrites(apiID, owner, "grant revoked")
	}
}

// ---- the app write path ------------------------------------------------------------------

// Target is an API an app may write to, resolved and authorized.
type Target struct {
	Def   apidef.Definition
	Model table.Model
	Grant store.Grant
	// Writable is the API field names the app may set: the API's columns narrowed by the grant.
	Writable map[string]bool
}

// Allows reports whether the app may make op on the target.
func (t Target) Allows(op string) bool {
	return t.Def.Writes.Allows(op) && slices.Contains(t.Grant.Operations, op)
}

// Resolve finds the API slug names in the app's own workspace and its live grant there.
func (s Service) Resolve(ctx context.Context, c appauth.Caller, slug string) (Target, error) {
	def, err := s.Store.GetAPIBySlug(ctx, c.Workspace, slug)
	if errors.Is(err, store.ErrNotFound) {
		return Target{}, &table.WriteError{Status: 404, Code: table.CodeNotFound, Msg: "no API named " + slug}
	}
	if err != nil {
		return Target{}, err
	}
	if def.Writes == nil {
		return Target{}, &table.WriteError{Status: 403, Code: table.CodeOperationNotAllowed, Msg: "writes are off for this API"}
	}
	g, err := s.Store.LiveGrant(ctx, c.Workspace, def.ID, c.Subject)
	if errors.Is(err, store.ErrNotFound) {
		return Target{}, &table.WriteError{Status: 403, Code: table.CodeWriteNotGranted, Msg: "this app has no write grant on the " + slug + " API"}
	}
	if err != nil {
		return Target{}, err
	}
	m := table.NewModel(def)
	writable := map[string]bool{}
	for _, col := range def.Writes.Columns {
		if g.Columns != nil && !slices.Contains(g.Columns, col) {
			continue
		}
		if f, ok := m.FieldForColumn(col); ok {
			writable[f.Name] = true
		}
	}
	return Target{Def: def, Model: m, Grant: g, Writable: writable}, nil
}

// AppWrite is one write by an app.
type AppWrite struct {
	Caller appauth.Caller
	// ReportedUser is the X-Booth-App-User booth-streamlit's gate reported: audited as reported,
	// never used for authorization.
	ReportedUser   string
	Request        table.WriteRequest
	IdempotencyKey string
}

// Outcome is a successful write (or its replay).
type Outcome struct {
	Status   int       `json:"status"`
	Row      table.Row `json:"row"`
	Version  string    `json:"version"`
	Replayed bool      `json:"-"`
}

// Execute runs one write on a resolved target. Every attempt that gets this far is audited.
func (s Service) Execute(ctx context.Context, t Target, w AppWrite) (out Outcome, err error) {
	start := s.now()
	entry := store.WriteLogEntry{Workspace: t.Def.Workspace, APIID: t.Def.ID, Operation: w.Request.Op, Principal: w.Caller.Subject,
		GrantedBy: t.Grant.GrantedBy, ReportedUser: w.ReportedUser, IdempotencyKey: w.IdempotencyKey}
	defer func() {
		entry.Duration = s.now().Sub(start)
		entry.Status, entry.Outcome = outcomeOf(out, err)
		if lerr := s.Store.LogWrite(context.WithoutCancel(ctx), entry); lerr != nil {
			log.Printf("writes: audit log for API %s: %v", t.Def.ID, lerr)
		}
		// Row values are never logged: the line names the API, the operation, the app and the result.
		log.Printf("writes: %s api=%s workspace=%s principal=%s grantedBy=%s status=%d outcome=%s", w.Request.Op, t.Def.ID, t.Def.Workspace,
			w.Caller.Subject, t.Grant.GrantedBy, entry.Status, entry.Outcome)
	}()

	if !t.Allows(w.Request.Op) {
		return Outcome{}, &table.WriteError{Status: 403, Code: table.CodeOperationNotAllowed, Msg: w.Request.Op + " isn't allowed for this app on this API"}
	}
	if (w.Request.Op == apidef.OpInsert || w.Request.Op == apidef.OpUpdate) && len(t.Writable) == 0 {
		return Outcome{}, &table.WriteError{Status: 403, Code: table.CodeNoWritableColumns, Msg: "no column is writable for this app; an owner must re-save the API's write settings"}
	}
	if ok, wait := s.Limiter.Allow(w.Caller.Subject); !ok {
		return Outcome{}, &RateLimited{Wait: wait}
	}
	if !s.dataAccess() {
		return Outcome{}, &table.WriteError{Status: 503, Code: table.CodeUnavailable, Msg: ErrDataAccessOff.Error()}
	}
	req := w.Request
	req.Writable = t.Writable

	if w.IdempotencyKey != "" {
		// Assigned, not declared: the deferred function below must see Execute's own err.
		var replay Outcome
		var claimed bool
		replay, claimed, err = s.claim(ctx, t, w)
		if err != nil || !claimed {
			return replay, err
		}
		defer func() {
			bg := context.WithoutCancel(ctx)
			switch {
			case err == nil:
				b, _ := json.Marshal(out)
				if ferr := s.Store.FinishIdempotencyKey(bg, t.Def.ID, w.Caller.Subject, w.IdempotencyKey, b); ferr != nil {
					log.Printf("writes: recording idempotency key for API %s: %v", t.Def.ID, ferr)
				}
			case errors.Is(err, table.ErrOutcomeUnknown):
				// Leave it pending: a retry with this key answers idempotency_outcome_unknown
				// rather than risk writing the row twice.
			default:
				if rerr := s.Store.ReleaseIdempotencyKey(bg, t.Def.ID, w.Caller.Subject, w.IdempotencyKey); rerr != nil {
					log.Printf("writes: releasing idempotency key for API %s: %v", t.Def.ID, rerr)
				}
			}
		}()
	}

	res, err := s.write(ctx, t, w.Caller.Subject, req)
	if err != nil {
		return Outcome{}, err
	}
	if len(res.KeyText) > 0 {
		sum := sha256.Sum256([]byte(strings.Join(res.KeyText, "\x00")))
		entry.RowKeyHash = hex.EncodeToString(sum[:])
	}
	status := 200
	if req.Op == apidef.OpInsert {
		status = 201
	}
	return Outcome{Status: status, Row: res.Row, Version: res.Version}, nil
}

// write runs the statement on the granting owner's write sidecar. On SQLSTATE 42501 it reissues
// the full scope once (booth-database brings the write group back in line with the settings),
// restarts the sidecar and retries once.
func (s Service) write(ctx context.Context, t Target, principal string, req table.WriteRequest) (table.WriteResult, error) {
	spec := sidecars.WriteSpec{Workspace: t.Def.Workspace, APIID: t.Def.ID, Owner: t.Grant.GrantedBy, Scope: Scope(t.Def), Version: t.Def.Writes.Version}
	pool, err := s.Pools.WritePool(ctx, spec)
	if err != nil {
		return table.WriteResult{}, poolError(err)
	}
	res, err := table.Write(ctx, pool, t.Model, req, s.Timeout)
	if !errors.Is(err, table.ErrInsufficientPrivilege) {
		return res, err
	}
	log.Printf("writes: API %s: the write lease lacked a privilege (42501); reissuing the full scope once", t.Def.ID)
	if rerr := s.reissue(ctx, t.Def, t.Grant.GrantedBy, principal); rerr != nil {
		return table.WriteResult{}, poolError(rerr)
	}
	s.Pools.StopWrites(t.Def.ID, t.Grant.GrantedBy, "reissued after 42501")
	if pool, err = s.Pools.WritePool(ctx, spec); err != nil {
		return table.WriteResult{}, poolError(err)
	}
	res, err = table.Write(ctx, pool, t.Model, req, s.Timeout)
	if errors.Is(err, table.ErrInsufficientPrivilege) {
		return res, &table.WriteError{Status: 503, Code: table.CodeUnavailable, Msg: "the database refused this write even after the write grant was reissued; ask an owner to re-save the API's write settings"}
	}
	return res, err
}

func poolError(err error) error {
	var we *table.WriteError
	var se *SettingsError
	switch {
	case errors.As(err, &we):
		return err
	case errors.Is(err, source.ErrOwnerNoAccess):
		return &table.WriteError{Status: 403, Code: table.CodeOwnerNoAccess,
			Msg: "the owner who granted these writes no longer has the editor or owner role in this workspace, or hasn't signed in within 7 days; an owner must grant again"}
	case errors.As(err, &se):
		return &table.WriteError{Status: 503, Code: table.CodeUnavailable, Msg: se.Msg}
	case errors.Is(err, context.Canceled):
		return err
	}
	log.Printf("writes: no write lease: %v", err)
	return &table.WriteError{Status: 503, Code: table.CodeUnavailable, Msg: "no write lease is available right now; retry shortly"}
}

// claim takes the idempotency key, or answers from it: a replay, or a refusal.
func (s Service) claim(ctx context.Context, t Target, w AppWrite) (Outcome, bool, error) {
	if len(w.IdempotencyKey) > MaxIdempotencyKey {
		return Outcome{}, false, &table.WriteError{Status: 400, Code: table.CodeInvalidRequest, Msg: fmt.Sprintf("Idempotency-Key is longer than %d characters", MaxIdempotencyKey)}
	}
	hash := requestHash(w.Request)
	ttl := s.IdempotencyTTL
	if ttl <= 0 {
		ttl = DefaultIdempotencyTTL
	}
	rec, claimed, err := s.Store.ClaimIdempotencyKey(ctx, t.Def.ID, w.Caller.Subject, w.IdempotencyKey, hash, ttl)
	if err != nil || claimed {
		return Outcome{}, claimed, err
	}
	switch {
	case rec.RequestHash != hash:
		return Outcome{}, false, &table.WriteError{Status: 422, Code: table.CodeIdempotencyMismatch, Msg: "this Idempotency-Key was used with a different request"}
	case rec.State == "done":
		var out Outcome
		if err := json.Unmarshal(rec.Response, &out); err != nil {
			return Outcome{}, false, err
		}
		out.Replayed = true
		return out, false, nil
	case s.now().Sub(rec.CreatedAt) < 2*s.Timeout+30*time.Second:
		return Outcome{}, false, &table.WriteError{Status: 409, Code: table.CodeIdempotencyInFlight, Msg: "a request with this Idempotency-Key is still running; retry shortly"}
	}
	return Outcome{}, false, &table.WriteError{Status: 409, Code: table.CodeIdempotencyUnknown,
		Msg: "a request with this Idempotency-Key was interrupted and may or may not have been applied; check the table before writing again with a new key"}
}

func requestHash(r table.WriteRequest) string {
	b, _ := json.Marshal(map[string]any{"op": r.Op, "key": r.Key, "data": r.Data, "expectedVersion": r.ExpectedVersion})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RateLimited is a refused write over the principal's rate.
type RateLimited struct{ Wait time.Duration }

func (e *RateLimited) Error() string { return "too many writes from this app; slow down" }

func outcomeOf(out Outcome, err error) (int, string) {
	var we *table.WriteError
	var rl *RateLimited
	switch {
	case err == nil && out.Replayed:
		return out.Status, "replayed"
	case err == nil:
		return out.Status, "ok"
	case errors.As(err, &we):
		return we.Status, we.Code
	case errors.As(err, &rl):
		return 429, table.CodeRateLimited
	case errors.Is(err, table.ErrOutcomeUnknown):
		return 503, "outcome_unknown"
	case errors.Is(err, context.Canceled):
		return 499, "client_closed"
	}
	return 500, "internal_error"
}

// LogResolveRefusal audits a write refused before Execute (writes off, no grant), when the API
// is known.
func (s Service) LogResolveRefusal(ctx context.Context, c appauth.Caller, slug, op string, err error) {
	var we *table.WriteError
	if !errors.As(err, &we) || we.Code == table.CodeNotFound {
		return
	}
	def, gerr := s.Store.GetAPIBySlug(ctx, c.Workspace, slug)
	if gerr != nil {
		return
	}
	_ = s.Store.LogWrite(context.WithoutCancel(ctx), store.WriteLogEntry{Workspace: c.Workspace, APIID: def.ID, Operation: op,
		Principal: c.Subject, Status: we.Status, Outcome: we.Code})
	log.Printf("writes: %s api=%s workspace=%s principal=%s status=%d outcome=%s", op, def.ID, c.Workspace, c.Subject, we.Status, we.Code)
}

// WriteLog returns an API's recent audited writes.
func (s Service) WriteLog(ctx context.Context, workspace, apiID string, limit int) ([]store.WriteLogRecord, error) {
	if _, err := s.Store.GetAPI(ctx, workspace, apiID); err != nil {
		return nil, err
	}
	return s.Store.WriteLog(ctx, workspace, apiID, limit)
}
