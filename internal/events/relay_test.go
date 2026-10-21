package events_test

// The outbox relay end to end: booth-api's store on the test PostgreSQL, a real in-process
// nats-server with JetStream and a BOOTH_EVENTS stream shaped like booth-core's (ADR 0026), and the
// real Bus. Events queued while the bus is down are published once it is back, each exactly once.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/projectbooth/booth-api/internal/db/dbtest"
	"github.com/projectbooth/booth-api/internal/events"
	"github.com/projectbooth/booth-api/internal/store"
)

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *store.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.Pool(t)
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, pool: pool, store: store.New(pool)}
}

// queue records a committed write with its event, as the write path does.
func (e *env) queue(workspace, slug string) {
	e.t.Helper()
	data, _ := json.Marshal(events.WriteCommittedData{APISlug: slug, Dataset: "ds-1", Table: "finance.entries", Operation: "insert", Rows: 1, Principal: "streamlit:" + workspace + ":budget"})
	if err := e.store.RecordWrite(context.Background(),
		store.WriteLogEntry{Workspace: workspace, APIID: "api-1", Operation: "insert", Principal: "streamlit:" + workspace + ":budget", Status: 201, Outcome: "ok"},
		nil, &store.OutboxEvent{Workspace: workspace, EventType: events.WriteCommitted, Data: data}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) queued() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM event_outbox`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// natsBus is a nats-server that can be stopped and started again on the same port and storage,
// with a Bus connected to it.
type natsBus struct {
	t    *testing.T
	dir  string
	port int
	srv  *natsserver.Server
	bus  *events.Bus
}

func (b *natsBus) start() {
	b.t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: b.port, JetStream: true, StoreDir: b.dir, NoLog: true, NoSigs: true})
	if err != nil {
		b.t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		b.t.Fatal("nats-server did not start")
	}
	b.srv = srv
	b.port = srv.Addr().(*net.TCPAddr).Port
}

func (b *natsBus) stop() {
	b.srv.Shutdown()
	b.srv.WaitForShutdown()
	b.waitFor(false)
}

func (b *natsBus) waitFor(connected bool) {
	b.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for b.bus.Connected() != connected {
		if time.Now().After(deadline) {
			b.t.Fatalf("bus connected = %v, want %v", b.bus.Connected(), connected)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startBus runs nats-server with booth-core's stream, and a Bus connected to it.
func startBus(t *testing.T) *natsBus {
	t.Helper()
	b := &natsBus{t: t, dir: t.TempDir(), port: -1}
	b.start()
	t.Cleanup(func() { b.srv.Shutdown() })
	nc, err := nats.Connect(b.srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	js, _ := jetstream.New(nc)
	_, err = js.CreateStream(context.Background(), jetstream.StreamConfig{Name: "BOOTH_EVENTS", Subjects: []string{"booth.>"}, Duplicates: 2 * time.Minute})
	nc.Close()
	if err != nil {
		t.Fatal(err)
	}
	b.bus = events.New(events.Config{URL: b.srv.ClientURL()})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.bus.Run(ctx)
	b.waitFor(true)
	return b
}

// stored returns every message in the stream.
func (b *natsBus) stored() []jetstream.Msg {
	b.t.Helper()
	nc, err := nats.Connect(b.srv.ClientURL())
	if err != nil {
		b.t.Fatal(err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	ctx := context.Background()
	st, err := js.Stream(ctx, "BOOTH_EVENTS")
	if err != nil {
		b.t.Fatal(err)
	}
	info, err := st.Info(ctx)
	if err != nil {
		b.t.Fatal(err)
	}
	if info.State.Msgs == 0 {
		return nil
	}
	cons, err := st.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{})
	if err != nil {
		b.t.Fatal(err)
	}
	batch, err := cons.FetchNoWait(int(info.State.Msgs))
	if err != nil {
		b.t.Fatal(err)
	}
	var out []jetstream.Msg
	for m := range batch.Messages() {
		out = append(out, m)
	}
	if len(out) != int(info.State.Msgs) {
		b.t.Fatalf("read %d of %d stored messages", len(out), info.State.Msgs)
	}
	return out
}

func TestRelayPublishesQueuedEventsOnce(t *testing.T) {
	e := newEnv(t)
	b := startBus(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC)
	r := events.NewRelay(e.store, b.bus)
	r.Now = func() time.Time { return at }

	e.queue("acme", "finance-dev-entries")
	e.queue("globex", "notes")
	if n, err := r.Flush(ctx); n != 2 || err != nil {
		t.Fatalf("flush = %d, %v", n, err)
	}
	if e.queued() != 0 {
		t.Errorf("%d events left in the outbox after their acks", e.queued())
	}
	msgs := b.stored()
	if len(msgs) != 2 {
		t.Fatalf("%d messages stored, want 2", len(msgs))
	}
	if msgs[0].Subject() != "booth.acme.api.write.committed" || msgs[1].Subject() != "booth.globex.api.write.committed" {
		t.Errorf("subjects = %s, %s", msgs[0].Subject(), msgs[1].Subject())
	}
	var env struct {
		Workspace, EventType, PublishedBy string
		PublishedAt                       time.Time
		Data                              map[string]any
	}
	if err := json.Unmarshal(msgs[0].Data(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Workspace != "acme" || env.EventType != "api.write.committed" || env.PublishedBy != "api" || !env.PublishedAt.Equal(at) {
		t.Errorf("envelope = %+v", env)
	}
	want := map[string]any{"apiSlug": "finance-dev-entries", "dataset": "ds-1", "table": "finance.entries", "operation": "insert", "rows": float64(1), "principal": "streamlit:acme:budget"}
	if len(env.Data) != len(want) {
		t.Errorf("data = %v, want exactly %v", env.Data, want)
	}
	for k, v := range want {
		if env.Data[k] != v {
			t.Errorf("data[%s] = %v, want %v", k, env.Data[k], v)
		}
	}
	id := msgs[0].Headers().Get(nats.MsgIdHdr)
	if len(id) != len("api-")+36 || id[:4] != "api-" {
		t.Errorf("message id = %q, want api-<outbox msg_id>", id)
	}
	// A resend inside the duplicate window (an ack lost after the stream stored the event) is
	// stored once.
	if err := b.bus.Publish(ctx, msgs[0].Subject(), msgs[0].Data(), id); err != nil {
		t.Fatal(err)
	}
	if n := len(b.stored()); n != 2 {
		t.Errorf("%d messages after a resend with the same id, want 2", n)
	}
	if n, err := r.Flush(ctx); n != 0 || err != nil {
		t.Errorf("a second flush = %d, %v; want nothing to send", n, err)
	}
}

// The bus is down when the write commits: the event waits in the outbox, through a relay pass, and
// is published once the bus is back.
func TestRelayWaitsOutABusOutage(t *testing.T) {
	e := newEnv(t)
	b := startBus(t)
	ctx := context.Background()
	r := events.NewRelay(e.store, b.bus)

	b.stop()
	e.queue("acme", "entries")
	if n, err := r.Flush(ctx); n != 0 || !errors.Is(err, events.ErrNotConnected) {
		t.Errorf("flush with the bus down = %d, %v", n, err)
	}
	if e.queued() != 1 {
		t.Fatalf("%d queued after a failed publish, want 1", e.queued())
	}
	b.start()
	b.waitFor(true)
	// Run publishes without being woken (its periodic pass), as it must after an outage.
	r.Every = 50 * time.Millisecond
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go r.Run(runCtx)
	deadline := time.Now().Add(10 * time.Second)
	for e.queued() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the queued event was never published after the bus came back")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(b.stored()); n != 1 {
		t.Errorf("%d messages stored, want 1", n)
	}
}

// Wake publishes at once, without waiting for the periodic pass.
func TestRelayWake(t *testing.T) {
	e := newEnv(t)
	b := startBus(t)
	r := events.NewRelay(e.store, b.bus)
	r.Every = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	time.Sleep(100 * time.Millisecond) // Run's first pass found nothing; it now waits an hour
	e.queue("acme", "entries")
	r.Wake()
	deadline := time.Now().Add(5 * time.Second)
	for e.queued() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("Wake didn't publish the event")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type fakeBus struct {
	mu       sync.Mutex
	sent     []string
	failFrom int // fail every publish once this many have been sent; 0 never fails
	block    chan struct{}
	entered  chan struct{}
}

func (f *fakeBus) Connected() bool { return true }

func (f *fakeBus) Publish(_ context.Context, subject string, data []byte, msgID string) error {
	if f.block != nil {
		f.entered <- struct{}{}
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFrom > 0 && len(f.sent) >= f.failFrom {
		return errors.New("refused")
	}
	f.sent = append(f.sent, msgID)
	return nil
}

// A failed publish stops the pass: what was acknowledged is deleted, the rest stays queued in
// order and goes on the next pass.
func TestRelayStopsAtTheFirstFailure(t *testing.T) {
	e := newEnv(t)
	f := &fakeBus{failFrom: 1}
	r := events.NewRelay(e.store, f)
	for _, s := range []string{"a", "b", "c"} {
		e.queue("acme", s)
	}
	if n, err := r.Flush(context.Background()); n != 1 || err == nil {
		t.Fatalf("flush = %d, %v; want 1 sent then the refusal", n, err)
	}
	if e.queued() != 2 {
		t.Fatalf("%d queued, want 2", e.queued())
	}
	f.failFrom = 0
	if n, err := r.Flush(context.Background()); n != 2 || err != nil {
		t.Fatalf("second flush = %d, %v", n, err)
	}
	if len(f.sent) != 3 || f.sent[0] == f.sent[1] || f.sent[1] == f.sent[2] {
		t.Errorf("sent = %v, want three distinct ids", f.sent)
	}
}

// Two replicas relaying at once never send the same event: the second skips rows the first holds.
func TestRelayReplicasDontSendTwice(t *testing.T) {
	e := newEnv(t)
	e.queue("acme", "a")
	e.queue("acme", "b")
	slow := &fakeBus{block: make(chan struct{}), entered: make(chan struct{})}
	fast := &fakeBus{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := events.NewRelay(e.store, slow).Flush(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	<-slow.entered // the first replica holds both rows, mid-publish
	if n, err := events.NewRelay(e.store, fast).Flush(context.Background()); n != 0 || err != nil {
		t.Errorf("second replica sent %d (%v) while the first held the rows", n, err)
	}
	close(slow.block)
	go func() {
		for range slow.entered {
		}
	}()
	<-done
	close(slow.entered)
	if len(slow.sent) != 2 || len(fast.sent) != 0 || e.queued() != 0 {
		t.Errorf("first sent %v, second %v, %d left", slow.sent, fast.sent, e.queued())
	}
}

// Events not delivered within 24 hours are dropped.
func TestRelayExpiresOldEvents(t *testing.T) {
	e := newEnv(t)
	e.queue("acme", "old")
	e.queue("acme", "new")
	if _, err := e.pool.Exec(context.Background(), `UPDATE event_outbox SET created_at = now() - interval '25 hours' WHERE data->>'apiSlug' = 'old'`); err != nil {
		t.Fatal(err)
	}
	r := events.NewRelay(e.store, &fakeBus{})
	if n, err := r.Expire(context.Background()); n != 1 || err != nil {
		t.Fatalf("expire = %d, %v", n, err)
	}
	if e.queued() != 1 {
		t.Errorf("%d queued, want the recent one", e.queued())
	}
}
