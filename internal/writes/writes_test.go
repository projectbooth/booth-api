package writes_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/appauth"
	"github.com/projectbooth/booth-api/internal/auth"
	"github.com/projectbooth/booth-api/internal/ratelimit"
	"github.com/projectbooth/booth-api/internal/sidecars"
	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/store"
	"github.com/projectbooth/booth-api/internal/table"
	"github.com/projectbooth/booth-api/internal/writes"
	"github.com/projectbooth/booth-api/internal/writes/writestest"
)

var (
	alice = auth.Identity{Subject: "alice", DisplayName: "Alice", Workspace: "acme", Role: auth.RoleOwner}
	erin  = auth.Identity{Subject: "erin", DisplayName: "Erin", Workspace: "acme", Role: auth.RoleOwner}
	app   = appauth.Caller{Subject: "streamlit:acme:budget", Workspace: "acme", AppID: "budget", Role: "viewer"}
	app2  = appauth.Caller{Subject: "streamlit:acme:other", Workspace: "acme", AppID: "other", Role: "viewer"}
)

func wantCode(t *testing.T, err error, status int, code string) *table.WriteError {
	t.Helper()
	var we *table.WriteError
	if !errors.As(err, &we) || we.Status != status || we.Code != code {
		t.Fatalf("err = %#v (%v), want %d %s", we, err, status, code)
	}
	return we
}

// setup turns writes on (insert, update; amount, note, entered_by) and grants app insert+update.
func setup(t *testing.T) (*writestest.Env, apidef.Definition) {
	t.Helper()
	e := writestest.New(t)
	def := e.API("entries")
	ctx := context.Background()
	if _, err := e.Service.SaveSettings(ctx, alice, def.ID, []string{"update", "insert"}, []string{"note", "amount", "entered_by"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Service.Grant(ctx, alice, def.ID, app.Subject, []string{"insert", "update"}, nil); err != nil {
		t.Fatal(err)
	}
	return e, def
}

func write(t *testing.T, e *writestest.Env, c appauth.Caller, req table.WriteRequest, idem string) (writes.Outcome, error) {
	t.Helper()
	ctx := context.Background()
	target, err := e.Service.Resolve(ctx, c, "entries")
	if err != nil {
		return writes.Outcome{}, err
	}
	return e.Service.Execute(ctx, target, writes.AppWrite{Caller: c, ReportedUser: "household-member", Request: req, IdempotencyKey: idem})
}

func insert(data map[string]any) table.WriteRequest {
	return table.WriteRequest{Op: apidef.OpInsert, Data: data}
}

func TestSaveSettings(t *testing.T) {
	e := writestest.New(t)
	def := e.API("entries")
	ctx := context.Background()

	// Owners only is the HTTP layer's check; here, everything else.
	for name, tc := range map[string]struct {
		ops, cols []string
		want      string
	}{
		"no operations":              {nil, []string{"amount"}, "at least one operation"},
		"unknown operation":          {[]string{"truncate"}, nil, "unknown operation"},
		"required column not ticked": {[]string{"insert"}, []string{"amount"}, "entered_by"},
		"identity key":               {[]string{"insert"}, []string{"id", "amount", "entered_by"}, "primary-key column the database fills in"},
		"unknown column":             {[]string{"insert"}, []string{"amount", "entered_by", "nope"}, "not a column this API serves"},
		"delete with columns":        {[]string{"delete"}, []string{"note"}, "delete alone writes no columns"},
		"insert with no columns":     {[]string{"insert"}, nil, "at least one writable column"},
	} {
		_, err := e.Service.SaveSettings(ctx, alice, def.ID, tc.ops, tc.cols)
		var se *writes.SettingsError
		if !errors.As(err, &se) || !strings.Contains(se.Msg+problems(se), tc.want) {
			t.Errorf("%s: err = %v, want a SettingsError mentioning %q", name, err, tc.want)
		}
	}
	if e.Broker.Count() != 0 {
		t.Errorf("refused settings reached the broker %d times", e.Broker.Count())
	}

	v, err := e.Service.SaveSettings(ctx, alice, def.ID, []string{"insert", "update"}, []string{"note", "amount", "entered_by", "note"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Writes == nil || v.Writes.Version != 1 || !slices.Equal(v.Writes.Columns, []string{"amount", "note", "entered_by"}) || !slices.Equal(v.Writes.Operations, []string{"insert", "update"}) {
		t.Errorf("saved = %+v", v.Writes)
	}
	// The full scope went to the broker first, as the saving owner, on an apiwrite: token.
	call := e.Broker.Last()
	wantScope := `{"workspace":"acme","write":{"grant":"api:` + def.ID + `","tables":[{"columns":["amount","note","entered_by"],"name":"entries","operations":["insert","update"],"schema":"` + e.Schema + `"}]}}`
	if string(call.Scope) != wantScope || call.OnBehalfOf != "alice" || call.Token != "jwt:apiwrite:"+def.ID+":alice" || call.Revoke {
		t.Errorf("broker call = %+v\nscope %s\nwant  %s", call, call.Scope, wantScope)
	}
	if r := e.Minter.Requests[len(e.Minter.Requests)-1]; r.RoleCeiling != "editor" || r.Subject != "apiwrite:"+def.ID {
		t.Errorf("mint = %+v", r)
	}
	if e.Pools.StopCount() != 1 {
		t.Errorf("write sidecars not restarted on save: %v", e.Pools.Stops)
	}
	v, _ = e.Service.SaveSettings(ctx, alice, def.ID, []string{"insert"}, []string{"amount", "entered_by"})
	if v.Writes.Version != 2 {
		t.Errorf("version after a second save = %d", v.Writes.Version)
	}

	// booth-database refusing the scope refuses the save, and nothing is stored.
	e.Broker.Err = errors.New("unreachable")
	if _, err := e.Service.SaveSettings(ctx, alice, def.ID, []string{"insert"}, []string{"amount", "entered_by", "note"}); err == nil {
		t.Error("a save went through with the broker failing")
	}
	got, _ := e.Service.Settings(ctx, "acme", def.ID)
	if got.Writes.Version != 2 || slices.Contains(got.Writes.Columns, "note") {
		t.Errorf("a refused save changed the settings: %+v", got.Writes)
	}
}

func problems(se *writes.SettingsError) string {
	var b strings.Builder
	for _, p := range se.Problems {
		b.WriteString(p.Field + ": " + p.Problem + "; ")
	}
	return b.String()
}

func TestSettingsNeedACurrentSnapshotAndAKeyForUpdates(t *testing.T) {
	e := writestest.New(t)
	ctx := context.Background()
	def := e.API("other") // no primary key
	if _, err := e.Service.SaveSettings(ctx, alice, def.ID, []string{"update"}, []string{"x"}); err == nil || !strings.Contains(err.Error(), "primary key") {
		t.Errorf("update on a keyless table: %v", err)
	}
	if _, err := e.Service.SaveSettings(ctx, alice, def.ID, []string{"insert"}, []string{"x"}); err != nil {
		t.Errorf("insert on a keyless table: %v", err)
	}
	if _, err := e.Pool.Exec(ctx, `UPDATE apis SET snapshot_version = 1 WHERE id = $1`, def.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Service.SaveSettings(ctx, alice, def.ID, []string{"insert"}, []string{"x"}); !errors.Is(err, writes.ErrRegenerate) {
		t.Errorf("an old snapshot: %v, want ErrRegenerate", err)
	}
}

func TestGrants(t *testing.T) {
	e := writestest.New(t)
	def := e.API("entries")
	ctx := context.Background()
	if _, err := e.Service.Grant(ctx, alice, def.ID, app.Subject, nil, nil); !errors.Is(err, store.ErrWritesOff) {
		t.Errorf("grant with writes off: %v", err)
	}
	if _, err := e.Service.SaveSettings(ctx, alice, def.ID, []string{"insert", "update"}, []string{"amount", "entered_by", "note"}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]error{
		"key:abc":                 writes.ErrWriteKeysNotBuilt,
		"streamlit:globex:budget": writes.ErrBadPrincipal,
		"streamlit:acme:sources":  writes.ErrBadPrincipal,
		"alice":                   writes.ErrBadPrincipal,
	} {
		if _, err := e.Service.Grant(ctx, alice, def.ID, p, nil, nil); !errors.Is(err, want) {
			t.Errorf("principal %q: err = %v, want %v", p, err, want)
		}
	}
	if _, err := e.Service.Grant(ctx, alice, def.ID, app.Subject, []string{"delete"}, nil); err == nil {
		t.Error("a grant widened the API's operations")
	}
	if _, err := e.Service.Grant(ctx, alice, def.ID, app.Subject, nil, []string{"secret"}); err == nil {
		t.Error("a grant widened the API's columns")
	}
	g, err := e.Service.Grant(ctx, alice, def.ID, app.Subject, nil, nil)
	if err != nil || !slices.Equal(g.Operations, []string{"insert"}) || g.Columns != nil || g.GrantedBy != "alice" {
		t.Fatalf("default grant = %+v, %v (want insert only, the API's columns)", g, err)
	}
	// Granting again replaces: one live grant per app and API.
	g2, err := e.Service.Grant(ctx, erin, def.ID, app.Subject, []string{"insert", "update"}, []string{"amount", "entered_by"})
	if err != nil {
		t.Fatal(err)
	}
	live, _ := e.Service.Grants(ctx, "acme", store.GrantFilter{APIID: def.ID})
	if len(live) != 1 || live[0].ID != g2.ID || live[0].GrantedBy != "erin" {
		t.Errorf("live grants = %+v", live)
	}
	all, _ := e.Service.Grants(ctx, "acme", store.GrantFilter{IncludeRevoked: true})
	if len(all) != 2 {
		t.Errorf("with revoked: %d grants", len(all))
	}
	// alice's sidecar for this API is stopped: she no longer backs any grant on it.
	if !slices.Contains(e.Pools.Stops, def.ID+"/alice/grant revoked") {
		t.Errorf("stops = %v", e.Pools.Stops)
	}
	// Another workspace can't see or revoke it.
	other := auth.Identity{Subject: "gl", Workspace: "globex", Role: auth.RoleOwner}
	if gs, _ := e.Service.Grants(ctx, "globex", store.GrantFilter{}); len(gs) != 0 {
		t.Errorf("globex sees %d grants", len(gs))
	}
	if _, err := e.Service.RevokeGrant(ctx, other, g2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("globex revoking acme's grant: %v", err)
	}
}

func TestInsertAndAudit(t *testing.T) {
	e, def := setup(t)
	out, err := write(t, e, app, insert(map[string]any{"amount": "12.50", "note": "lunch", "entered_by": "sam"}), "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != 201 || string(out.Row["id"]) != `"1"` || string(out.Row["amount"]) != `"12.50"` || string(out.Row["note"]) != `"lunch"` ||
		len(out.Row["booked"]) == 0 || len(out.Version) != 64 {
		t.Errorf("outcome = %+v", out)
	}
	logs, err := e.Service.WriteLog(context.Background(), "acme", def.ID, 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("audit = %+v, %v", logs, err)
	}
	l := logs[0]
	if l.Operation != "insert" || l.Principal != app.Subject || l.GrantedBy != "alice" || l.ReportedUser != "household-member" || l.Status != 201 || l.Outcome != "ok" || len(l.RowKeyHash) != 64 {
		t.Errorf("audit entry = %+v", l)
	}
	// Row values are never recorded.
	var dump string
	if err := e.Pool.QueryRow(context.Background(), `SELECT string_agg(to_jsonb(w)::text, ' ') FROM write_log w`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"lunch", "12.50", "sam"} {
		if strings.Contains(dump, v) {
			t.Errorf("the audit log holds the row value %q: %s", v, dump)
		}
	}
}

func TestValidation(t *testing.T) {
	e, _ := setup(t)
	_, err := write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam", "secret": "x", "nope": 1}), "")
	we := wantCode(t, err, 422, table.CodeUnknownColumn) // the first problem, by field name, gives the code
	if len(we.Fields) != 2 || we.Fields[0].Field != "nope" || we.Fields[0].Problem != table.CodeUnknownColumn || we.Fields[1].Problem != table.CodeColumnNotWritable {
		t.Errorf("fields = %+v", we.Fields)
	}
	_, err = write(t, e, app, insert(map[string]any{"amount": "1"}), "")
	if we := wantCode(t, err, 422, table.CodeRequired); we.Fields[0].Field != "entered_by" {
		t.Errorf("fields = %+v", we.Fields)
	}
	_, err = write(t, e, app, insert(map[string]any{"amount": "lots", "entered_by": "sam"}), "")
	wantCode(t, err, 422, table.CodeInvalidValue)
	_, err = write(t, e, app, insert(map[string]any{"amount": nil, "entered_by": "sam"}), "")
	wantCode(t, err, 422, table.CodeInvalidValue)
	// Postgres's own checks: a CHECK constraint, a unique index, a numeric overflow.
	_, err = write(t, e, app, insert(map[string]any{"amount": "-1", "entered_by": "sam"}), "")
	if we := wantCode(t, err, 422, table.CodeConstraint); we.Constraint != "amount_positive" {
		t.Errorf("constraint = %q", we.Constraint)
	}
	if _, err := write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam", "note": "dup"}), ""); err != nil {
		t.Fatal(err)
	}
	_, err = write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam", "note": "dup"}), "")
	if we := wantCode(t, err, 409, table.CodeConflict); we.Constraint != "entries_note_unique" || strings.Contains(we.Msg, "dup") {
		t.Errorf("conflict = %+v (must name the constraint, never the value)", we)
	}
	_, err = write(t, e, app, insert(map[string]any{"amount": "12345678901234", "entered_by": "sam"}), "")
	wantCode(t, err, 422, table.CodeInvalidValue)
	// Delete isn't on for the API, nor in the grant.
	_, err = write(t, e, app, table.WriteRequest{Op: apidef.OpDelete, Key: map[string]any{"id": "1"}}, "")
	wantCode(t, err, 403, table.CodeOperationNotAllowed)
}

func TestUpdateWithVersions(t *testing.T) {
	e, _ := setup(t)
	out, err := write(t, e, app, insert(map[string]any{"amount": "5", "entered_by": "sam"}), "")
	if err != nil {
		t.Fatal(err)
	}
	upd := table.WriteRequest{Op: apidef.OpUpdate, Key: map[string]any{"id": "1"}, Data: map[string]any{"note": "fixed"}, ExpectedVersion: out.Version}
	out2, err := write(t, e, app, upd, "")
	if err != nil || out2.Status != 200 || string(out2.Row["note"]) != `"fixed"` || out2.Version == out.Version {
		t.Fatalf("update = %+v, %v", out2, err)
	}
	// The old version is stale now.
	_, err = write(t, e, app, upd, "")
	wantCode(t, err, 412, table.CodeVersionMismatch)
	// Without a version, last write wins.
	upd.ExpectedVersion = ""
	if _, err := write(t, e, app, upd, ""); err != nil {
		t.Errorf("unversioned update: %v", err)
	}
	_, err = write(t, e, app, table.WriteRequest{Op: apidef.OpUpdate, Key: map[string]any{"id": "99"}, Data: map[string]any{"note": "x"}}, "")
	wantCode(t, err, 404, table.CodeNotFound)
	_, err = write(t, e, app, table.WriteRequest{Op: apidef.OpUpdate, Key: map[string]any{"id": "1"}, Data: map[string]any{"secret": "x"}}, "")
	wantCode(t, err, 422, table.CodeColumnNotWritable)
	_, err = write(t, e, app, table.WriteRequest{Op: apidef.OpUpdate, Key: map[string]any{"amount": "1"}, Data: map[string]any{"note": "x"}}, "")
	wantCode(t, err, 400, table.CodeInvalidRequest)
}

func TestIdempotency(t *testing.T) {
	e, def := setup(t)
	ctx := context.Background()
	req := insert(map[string]any{"amount": json.Number("7"), "entered_by": "sam"})
	first, err := write(t, e, app, req, "k-1")
	if err != nil || first.Replayed {
		t.Fatal(err)
	}
	again, err := write(t, e, app, req, "k-1")
	if err != nil || !again.Replayed || string(again.Row["id"]) != string(first.Row["id"]) || again.Version != first.Version || again.Status != 201 {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	var n int
	_ = e.Pool.QueryRow(ctx, `SELECT count(*) FROM entries`).Scan(&n)
	if n != 1 {
		t.Errorf("%d rows after a replay, want 1", n)
	}
	// The same key with another body, or from another app (its own key space).
	_, err = write(t, e, app, insert(map[string]any{"amount": "8", "entered_by": "sam"}), "k-1")
	wantCode(t, err, 422, table.CodeIdempotencyMismatch)
	if _, err := e.Service.Grant(ctx, alice, def.ID, app2.Subject, nil, nil); err != nil {
		t.Fatal(err)
	}
	if out, err := write(t, e, app2, req, "k-1"); err != nil || out.Replayed {
		t.Errorf("another app's k-1: %+v, %v", out, err)
	}
	// A refused write releases its key, so the corrected retry may reuse it.
	_, err = write(t, e, app, insert(map[string]any{"amount": "-1", "entered_by": "sam"}), "k-2")
	wantCode(t, err, 422, table.CodeConstraint)
	if _, err := write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam"}), "k-2"); err != nil {
		t.Errorf("retry with a released key: %v", err)
	}
	// A key left pending: in flight while recent, unknown once old.
	h := `0000`
	if _, err := e.Pool.Exec(ctx, `INSERT INTO write_idempotency (api_id, principal, key, request_hash, state) VALUES ($1, $2, 'k-3', $3, 'pending')`,
		def.ID, app.Subject, h); err != nil {
		t.Fatal(err)
	}
	_, err = write(t, e, app, req, "k-3")
	wantCode(t, err, 422, table.CodeIdempotencyMismatch) // hash differs: mismatch wins
	if _, err := e.Pool.Exec(ctx, `UPDATE write_idempotency SET request_hash = $1 WHERE key = 'k-3'`, hashOf(t, e, def, req)); err != nil {
		t.Fatal(err)
	}
	_, err = write(t, e, app, req, "k-3")
	wantCode(t, err, 409, table.CodeIdempotencyInFlight)
	if _, err := e.Pool.Exec(ctx, `UPDATE write_idempotency SET created_at = now() - interval '5 minutes' WHERE key = 'k-3'`); err != nil {
		t.Fatal(err)
	}
	_, err = write(t, e, app, req, "k-3")
	wantCode(t, err, 409, table.CodeIdempotencyUnknown)
}

// hashOf returns the request hash a key was stored with, by claiming a throwaway key.
func hashOf(t *testing.T, e *writestest.Env, def apidef.Definition, req table.WriteRequest) string {
	t.Helper()
	if _, err := write(t, e, app, req, "probe"); err != nil {
		t.Fatal(err)
	}
	var h string
	if err := e.Pool.QueryRow(context.Background(), `SELECT request_hash FROM write_idempotency WHERE key = 'probe'`).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRateLimit(t *testing.T) {
	e, _ := setup(t)
	e.Service.Limiter = ratelimit.New(0.001, 2)
	for i := 0; i < 2; i++ {
		if _, err := write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam"}), ""); err != nil {
			t.Fatal(err)
		}
	}
	_, err := write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam"}), "")
	var rl *writes.RateLimited
	if !errors.As(err, &rl) || rl.Wait <= 0 {
		t.Errorf("third write: %v, want RateLimited", err)
	}
}

// The write group lagging the settings (here: INSERT taken away behind booth-api's back) makes the
// write fail with 42501; booth-api reissues the full scope once, as the app, and the retry works.
func TestReissueOn42501(t *testing.T) {
	e, def := setup(t)
	if _, err := write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam"}), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Exec(context.Background(), `REVOKE INSERT ON entries FROM `+e.Role); err != nil {
		t.Fatal(err)
	}
	before := e.Broker.Count()
	if _, err := write(t, e, app, insert(map[string]any{"amount": "2", "entered_by": "sam"}), ""); err != nil {
		t.Fatalf("write after the group lost INSERT: %v", err)
	}
	if e.Broker.Count() != before+1 {
		t.Errorf("broker calls: %d, want exactly one reissue", e.Broker.Count()-before)
	}
	if c := e.Broker.Last(); c.OnBehalfOf != app.Subject || c.Token != "jwt:apiwrite:"+def.ID+":alice" {
		t.Errorf("reissue = %+v (want onBehalfOf the app, minted for the granting owner)", c)
	}
	// If the reissue doesn't help, one retry only, then 503.
	if _, err := e.Pool.Exec(context.Background(), `REVOKE INSERT ON entries FROM `+e.Role); err != nil {
		t.Fatal(err)
	}
	e.Broker.Ignore = true
	before = e.Broker.Count()
	_, err := write(t, e, app, insert(map[string]any{"amount": "3", "entered_by": "sam"}), "")
	wantCode(t, err, 503, table.CodeUnavailable)
	if e.Broker.Count() != before+1 {
		t.Errorf("broker calls: %d, want exactly one reissue", e.Broker.Count()-before)
	}
}

func TestOnlyTheGrantedTableAndNoDDL(t *testing.T) {
	e, def := setup(t)
	ctx := context.Background()
	def, err := e.Store.GetAPI(ctx, "acme", def.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := e.Pools.WritePool(ctx, sidecars.WriteSpec{Workspace: "acme", APIID: def.ID, Owner: "alice", Scope: writes.Scope(def), Version: def.Writes.Version})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`INSERT INTO other VALUES (1)`, `CREATE TABLE sneaky (x int)`, `UPDATE entries SET secret = 'x'`, `DROP TABLE entries`} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("the write group ran %q", q)
		}
	}
}

func TestRevocation(t *testing.T) {
	e, def := setup(t)
	ctx := context.Background()
	if _, err := e.Service.Grant(ctx, alice, def.ID, app2.Subject, nil, nil); err != nil {
		t.Fatal(err)
	}
	live, _ := e.Service.Grants(ctx, "acme", store.GrantFilter{Principal: app.Subject})
	if _, err := e.Service.RevokeGrant(ctx, alice, live[0].ID); err != nil {
		t.Fatal(err)
	}
	_, err := write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam"}), "")
	wantCode(t, err, 403, table.CodeWriteNotGranted)
	// alice still backs app2's grant, so her sidecar keeps running.
	if slices.Contains(e.Pools.Stops, def.ID+"/alice/grant revoked") {
		t.Errorf("stops = %v", e.Pools.Stops)
	}
	gs, err := e.Service.RevokePrincipal(ctx, alice, app2.Subject)
	if err != nil || len(gs) != 1 {
		t.Fatalf("revoke principal = %+v, %v", gs, err)
	}
	if !slices.Contains(e.Pools.Stops, def.ID+"/alice/grant revoked") {
		t.Errorf("the last grant went but the sidecar wasn't stopped: %v", e.Pools.Stops)
	}
	if gs, _ := e.Service.RevokePrincipal(ctx, alice, app2.Subject); len(gs) != 0 {
		t.Errorf("revoking twice revoked %d", len(gs))
	}
	if _, err := e.Service.RevokePrincipal(ctx, alice, "streamlit:globex:x"); !errors.Is(err, writes.ErrBadPrincipal) {
		t.Errorf("another workspace's app: %v", err)
	}
}

func TestDisableWrites(t *testing.T) {
	e, def := setup(t)
	ctx := context.Background()
	res, err := e.Service.DisableWrites(ctx, alice, def.ID)
	if err != nil || res.RevokedGrants != 1 || !res.DatabaseRevoked {
		t.Fatalf("disable = %+v, %v", res, err)
	}
	if c := e.Broker.Last(); !c.Revoke || c.Grant != "api:"+def.ID || c.OnBehalfOf != "alice" {
		t.Errorf("revoke request = %+v", c)
	}
	if !slices.Contains(e.Pools.Stops, def.ID+"//writes turned off") {
		t.Errorf("stops = %v", e.Pools.Stops)
	}
	_, err = write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam"}), "")
	wantCode(t, err, 403, table.CodeOperationNotAllowed)
	// Turning writes on again doesn't bring the old grant back.
	if _, err := e.Service.SaveSettings(ctx, alice, def.ID, []string{"insert"}, []string{"amount", "entered_by"}); err != nil {
		t.Fatal(err)
	}
	_, err = write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam"}), "")
	wantCode(t, err, 403, table.CodeWriteNotGranted)
	// Turning off twice is a no-op; the broker isn't called again.
	n := e.Broker.Count()
	_, _ = e.Service.DisableWrites(ctx, alice, def.ID)
	if res, _ := e.Service.DisableWrites(ctx, alice, def.ID); res.DatabaseRevoked || e.Broker.Count() != n+1 {
		t.Errorf("second disable = %+v, broker calls %d", res, e.Broker.Count()-n)
	}
}

// Removing the granting owner makes the next mint fail: writes stop with a clear 403.
func TestGrantingOwnerLosesAccess(t *testing.T) {
	e, _ := setup(t)
	e.Minter.Refuse = map[string]bool{"alice": true}
	_, err := write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam"}), "")
	wantCode(t, err, 403, table.CodeOwnerNoAccess)
}

func TestDeleteNeedsOwnerWhenWritable(t *testing.T) {
	e, def := setup(t)
	ctx := context.Background()
	editor := auth.Identity{Subject: "ed", Workspace: "acme", Role: auth.RoleEditor}
	if err := e.Service.BeforeDeleteAPI(ctx, editor, def.ID); !errors.Is(err, writes.ErrDeleteWritable) {
		t.Errorf("editor deleting a writable API: %v", err)
	}
	if err := e.Service.BeforeDeleteAPI(ctx, alice, def.ID); err != nil {
		t.Fatal(err)
	}
	if c := e.Broker.Last(); !c.Revoke {
		t.Errorf("deleting a writable API didn't send the revoke request: %+v", c)
	}
}

func TestRegenerateNeverWidens(t *testing.T) {
	e, def := setup(t)
	ctx := context.Background()
	if _, err := e.Pool.Exec(ctx, `ALTER TABLE entries ADD COLUMN category text; ALTER TABLE entries DROP COLUMN note`); err != nil {
		t.Fatal(err)
	}
	tbl, err := source.Introspect(ctx, e.Pool, def.Table)
	if err != nil {
		t.Fatal(err)
	}
	d := def
	d.Columns, d.PrimaryKey = tbl.Columns, tbl.PrimaryKey
	got, err := e.Store.UpdateSnapshot(ctx, d, table.NewModel(d).WritableCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Writes.Columns, []string{"amount", "entered_by"}) || got.Writes.Version != 2 {
		t.Errorf("after regenerate: %+v (note dropped, category not added, version bumped)", got.Writes)
	}
}
