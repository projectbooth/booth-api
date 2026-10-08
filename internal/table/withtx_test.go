package table

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-api/internal/db/dbtest"
)

func testPool(t *testing.T) *pgxpool.Pool { return dbtest.Pool(t) }

// flakyDB fails BeginTx with the given errors, in order, then delegates.
type flakyDB struct {
	errs  []error
	calls int
	next  DB
}

func (f *flakyDB) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}
	return f.next.BeginTx(ctx, o)
}

func TestWithTxRetriesADroppedConnectionOnce(t *testing.T) {
	pool := testPool(t)
	ran := 0
	fn := func(pgx.Tx) error { ran++; return nil }

	db := &flakyDB{errs: []error{io.ErrUnexpectedEOF}, next: pool}
	if err := WithTx(context.Background(), db, time.Second, fn); err != nil || db.calls != 2 || ran != 1 {
		t.Errorf("one dropped connection: err=%v calls=%d ran=%d", err, db.calls, ran)
	}
	db = &flakyDB{errs: []error{io.ErrUnexpectedEOF, io.ErrUnexpectedEOF}, next: pool}
	if err := WithTx(context.Background(), db, time.Second, fn); !errors.Is(err, io.ErrUnexpectedEOF) || db.calls != 2 {
		t.Errorf("two in a row: err=%v calls=%d (retried more than once?)", err, db.calls)
	}
	// An error the server reported is an answer, not a dropped connection.
	db = &flakyDB{errs: []error{&pgconn.PgError{Code: "25006"}}, next: pool}
	if err := WithTx(context.Background(), db, time.Second, fn); err == nil || db.calls != 1 {
		t.Errorf("server error: err=%v calls=%d", err, db.calls)
	}
	// A failure inside fn is never retried.
	calls := 0
	_ = WithTx(context.Background(), pool, time.Second, func(pgx.Tx) error { calls++; return io.ErrUnexpectedEOF })
	if calls != 1 {
		t.Errorf("fn ran %d times", calls)
	}
}
