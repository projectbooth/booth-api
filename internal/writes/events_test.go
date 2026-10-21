package writes_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/projectbooth/booth-api/internal/apidef"
	"github.com/projectbooth/booth-api/internal/ratelimit"
	"github.com/projectbooth/booth-api/internal/store"
	"github.com/projectbooth/booth-api/internal/table"
	"github.com/projectbooth/booth-api/internal/writes"
	"github.com/projectbooth/booth-api/internal/writes/writestest"
)

// outbox returns the queued events' data, oldest first.
func outbox(t *testing.T, e *writestest.Env) []map[string]any {
	t.Helper()
	rows, err := e.Pool.Query(context.Background(), `SELECT workspace, event_type, data FROM event_outbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var ws, typ string
		var data map[string]any
		if err := rows.Scan(&ws, &typ, &data); err != nil {
			t.Fatal(err)
		}
		if ws != "acme" || typ != "api.write.committed" {
			t.Errorf("queued %s in %s", typ, ws)
		}
		out = append(out, data)
	}
	return out
}

// ADR 0123: one api.write.committed per committed write, queued with the write's audit entry and
// idempotency result; nothing for a write that didn't commit, a replay, or with events off.
func TestEventsForCommittedWritesOnly(t *testing.T) {
	e := writestest.New(t)
	def := e.API("entries")
	ctx := context.Background()
	if _, err := e.Service.SaveSettings(ctx, alice, def.ID, []string{"insert", "update", "delete"}, []string{"note", "amount", "entered_by"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Service.Grant(ctx, alice, def.ID, app.Subject, []string{"insert", "update", "delete"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Service.Grant(ctx, alice, def.ID, app2.Subject, []string{"insert"}, nil); err != nil {
		t.Fatal(err)
	}
	wakes := 0
	e.Service.Events, e.Service.WakeRelay = true, func() { wakes++ }

	committed := func(what string, op string, c string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		q := outbox(t, e)
		want := map[string]any{"apiSlug": def.Slug, "dataset": def.DatasetID, "table": e.Schema + ".entries", "operation": op, "rows": float64(1), "principal": c}
		if len(q) == 0 || len(q[len(q)-1]) != len(want) {
			t.Fatalf("%s: outbox = %v, want a last event of exactly %v", what, q, want)
		}
		for k, v := range want {
			if q[len(q)-1][k] != v {
				t.Errorf("%s: event %s = %v, want %v", what, k, q[len(q)-1][k], v)
			}
		}
	}
	none := func(what string, before int) {
		t.Helper()
		if n := len(outbox(t, e)); n != before {
			t.Errorf("%s queued %d event(s), want none", what, n-before)
		}
	}

	// Committed: insert (with a key), update, delete.
	marker := map[string]any{"amount": "12.50", "note": "rs-marker-value", "entered_by": "marker-user"}
	first, err := write(t, e, app, insert(marker), "marker-key")
	committed("insert", "insert", app.Subject, err)
	var state string
	var resp json.RawMessage
	if err := e.Pool.QueryRow(ctx, `SELECT state, response FROM write_idempotency WHERE key = 'marker-key'`).Scan(&state, &resp); err != nil || state != "done" || len(resp) == 0 {
		t.Errorf("idempotency key after the write = %q, %s, %v", state, resp, err)
	}
	id := strings.Trim(string(first.Row["id"]), `"`)
	_, err = write(t, e, app, table.WriteRequest{Op: apidef.OpUpdate, Key: map[string]any{"id": id}, Data: map[string]any{"note": "rs-marker-value-2"}}, "")
	committed("update", "update", app.Subject, err)

	// Not committed, so no event: a replay, refusals, a missing row, a stale version, an operation
	// the app wasn't granted, the rate limit.
	n := len(outbox(t, e))
	again, err := write(t, e, app, insert(marker), "marker-key")
	if err != nil || !again.Replayed {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	none("a replay", n)
	_, err = write(t, e, app, insert(map[string]any{"amount": "1", "entered_by": "sam", "secret": "x"}), "")
	wantCode(t, err, 422, table.CodeColumnNotWritable)
	none("a column that isn't writable", n)
	_, err = write(t, e, app, insert(map[string]any{"amount": "-1", "entered_by": "sam"}), "k-refused")
	wantCode(t, err, 422, table.CodeConstraint)
	none("a constraint violation", n)
	_, err = write(t, e, app, table.WriteRequest{Op: apidef.OpUpdate, Key: map[string]any{"id": "999"}, Data: map[string]any{"note": "x"}}, "")
	wantCode(t, err, 404, table.CodeNotFound)
	none("an update of a missing row", n)
	_, err = write(t, e, app, table.WriteRequest{Op: apidef.OpUpdate, Key: map[string]any{"id": id}, Data: map[string]any{"note": "y"}, ExpectedVersion: first.Version}, "")
	wantCode(t, err, 412, table.CodeVersionMismatch)
	none("a stale version", n)
	_, err = write(t, e, app2, table.WriteRequest{Op: apidef.OpDelete, Key: map[string]any{"id": id}}, "")
	wantCode(t, err, 403, table.CodeOperationNotAllowed)
	none("an operation the app wasn't granted", n)
	e.Service.Limiter = ratelimit.New(0.001, 1)
	_, err = write(t, e, app2, insert(map[string]any{"amount": "1", "entered_by": "sam"}), "")
	committed("app2's insert", "insert", app2.Subject, err)
	n = len(outbox(t, e))
	_, err = write(t, e, app2, insert(map[string]any{"amount": "1", "entered_by": "sam"}), "")
	var rl *writes.RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("over the rate: %v", err)
	}
	none("a rate-limited write", n)
	e.Service.Limiter = ratelimit.New(1000, 1000)

	_, err = write(t, e, app, table.WriteRequest{Op: apidef.OpDelete, Key: map[string]any{"id": id}}, "")
	committed("delete", "delete", app.Subject, err)

	// The reissue after 42501 retries the write once: still one commit, one event.
	if _, err := e.Pool.Exec(ctx, `REVOKE INSERT ON entries FROM `+e.Role); err != nil {
		t.Fatal(err)
	}
	n = len(outbox(t, e))
	_, err = write(t, e, app, insert(map[string]any{"amount": "2", "entered_by": "sam"}), "")
	committed("insert after a reissue", "insert", app.Subject, err)
	if got := len(outbox(t, e)) - n; got != 1 {
		t.Errorf("the reissued insert queued %d events, want 1", got)
	}
	if total := len(outbox(t, e)); wakes != total || total != 5 {
		t.Errorf("%d events queued, relay woken %d times; want 5 and 5", total, wakes)
	}

	// The event never carries row values, key values, the reported user, the idempotency key or the
	// granting owner.
	var dump string
	if err := e.Pool.QueryRow(ctx, `SELECT string_agg(data::text, ' ') FROM event_outbox`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"rs-marker", "marker-user", "12.50", "household-member", "marker-key", "alice", `"id"`, `"1"`} {
		if strings.Contains(dump, v) {
			t.Errorf("an event holds %q: %s", v, dump)
		}
	}

	// Events off: the write is recorded as before, with no event.
	e.Service.Events = false
	n = len(outbox(t, e))
	if _, err := write(t, e, app, insert(map[string]any{"amount": "3", "entered_by": "sam"}), "k-off"); err != nil {
		t.Fatal(err)
	}
	none("a write with events off", n)
	logs, err := e.Service.WriteLog(ctx, "acme", def.ID, 100)
	if err != nil || !slices.ContainsFunc(logs, func(l store.WriteLogRecord) bool { return l.IdempotencyKey == "k-off" && l.Outcome == "ok" }) {
		t.Errorf("the write with events off isn't in the audit log: %v", err)
	}
}
