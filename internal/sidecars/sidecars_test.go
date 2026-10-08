package sidecars

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/workload"
)

// The fake sidecar is this test binary re-executed with BOOTH_FAKE_SIDECAR=1. It takes the real
// binary's flags, records them, answers GET /healthz on its socket, and relays everything else to
// the test PostgreSQL, so the manager's process, socket, pool and lifecycle handling run for real.
// The real credential-sidecar (its broker calls, wire protocol and lease) is exercised by the
// cross-module integration test against real booth-core and booth-database.
func TestMain(m *testing.M) {
	if os.Getenv("BOOTH_FAKE_SIDECAR") == "1" {
		fakeSidecar()
		return
	}
	os.Exit(m.Run())
}

func fakeSidecar() {
	fs := flag.NewFlagSet("fake", flag.ExitOnError)
	kind := fs.String("kind", "", "")
	access := fs.String("access", "", "")
	scope := fs.String("scope", "", "")
	workspace := fs.String("workspace", "", "")
	tokenFile := fs.String("token-file", "", "")
	listen := fs.String("listen", "", "")
	coreURL := fs.String("core-url", "", "")
	_ = fs.Parse(os.Args[1:])
	rec, _ := json.Marshal(map[string]any{
		"kind": *kind, "access": *access, "scope": *scope, "workspace": *workspace, "tokenFile": *tokenFile,
		"listen": *listen, "coreURL": *coreURL, "env": os.Environ(),
	})
	f, _ := os.OpenFile(os.Getenv("BOOTH_FAKE_RECORD"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	_, _ = f.Write(append(rec, '\n'))
	_ = f.Close()
	if os.Getenv("BOOTH_FAKE_FAIL") == "1" {
		fmt.Println("fake sidecar: refusing to start")
		os.Exit(1)
	}
	l, err := net.Listen("unix", strings.TrimPrefix(*listen, "unix://"))
	if err != nil {
		fmt.Println("fake sidecar:", err)
		os.Exit(1)
	}
	upstream := os.Getenv("BOOTH_FAKE_UPSTREAM")
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			br := bufio.NewReader(c)
			if p, _ := br.Peek(4); string(p) == "GET " {
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
				return
			}
			u, err := net.Dial("tcp", upstream)
			if err != nil {
				return
			}
			defer u.Close()
			go func() { _, _ = io.Copy(u, br) }()
			_, _ = io.Copy(c, u)
		}(c)
	}
}

type fakeMinter struct {
	mu    sync.Mutex
	ttl   time.Duration
	err   error
	calls []string
	n     int
}

func (f *fakeMinter) Mint(_ context.Context, ws, owner string) (workload.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ws+"/"+owner)
	if f.err != nil {
		return workload.Token{}, f.err
	}
	f.n++
	return workload.Token{JWT: fmt.Sprintf("jwt-%s-%s-%d", ws, owner, f.n), ExpiresAt: time.Now().Add(f.ttl), Role: "viewer"}, nil
}

func (f *fakeMinter) set(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeMinter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type harness struct {
	m      *Manager
	minter *fakeMinter
	record string
	dir    string
}

func newHarness(t *testing.T, cfg Config, env ...string) *harness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets and pgx's host=<dir> form: run on Linux (CI does)")
	}
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_POSTGRES") == "1" {
			t.Fatal("BOOTH_TEST_POSTGRES_DSN is not set")
		}
		t.Skip("BOOTH_TEST_POSTGRES_DSN not set")
	}
	u, _ := url.Parse(dsn)
	pw, _ := u.User.Password()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Short base path: a Unix socket path is limited to about 107 bytes.
	dir, err := os.MkdirTemp("/tmp", "bsc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	record := filepath.Join(dir, "record.jsonl")
	cfg.Binary, cfg.Dir, cfg.CoreURL = bin, filepath.Join(dir, "s"), "http://core.test"
	cfg.ExtraEnv = append([]string{"BOOTH_FAKE_SIDECAR=1", "BOOTH_FAKE_RECORD=" + record, "BOOTH_FAKE_UPSTREAM=" + u.Host}, env...)
	cfg.ConnString = func(d string) string {
		return fmt.Sprintf("host=%s port=5432 user=%s password=%s dbname=%s sslmode=disable", d, u.User.Username(), pw, strings.TrimPrefix(u.Path, "/"))
	}
	fm := &fakeMinter{ttl: time.Hour}
	m, err := New(cfg, fm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return &harness{m: m, minter: fm, record: record, dir: cfg.Dir}
}

func (h *harness) records(t *testing.T) []map[string]any {
	t.Helper()
	b, _ := os.ReadFile(h.record)
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func query(t *testing.T, h *harness, ws, owner string) {
	t.Helper()
	pool, err := h.m.Pool(context.Background(), ws, owner)
	if err != nil {
		t.Fatalf("Pool(%s, %s): %v", ws, owner, err)
	}
	var one int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("query through the sidecar: %v", err)
	}
}

func TestStartsOneSidecarPerWorkspaceAndOwner(t *testing.T) {
	h := newHarness(t, Config{})
	query(t, h, "acme", "alice")
	query(t, h, "acme", "alice")
	if h.m.Running() != 1 || h.minter.count() != 1 {
		t.Errorf("after two requests: %d running, %d mints; want one of each", h.m.Running(), h.minter.count())
	}
	query(t, h, "acme", "bob")
	query(t, h, "globex", "alice")
	if h.m.Running() != 3 {
		t.Errorf("running = %d, want 3 (acme/alice, acme/bob, globex/alice)", h.m.Running())
	}

	recs := h.records(t)
	if len(recs) != 3 {
		t.Fatalf("started %d sidecars", len(recs))
	}
	r := recs[0]
	dir := filepath.Join(h.dir, dirName(key{"acme", "alice"}))
	want := map[string]any{
		"kind": "postgres", "access": "read", "scope": `{"workspace":"acme"}`, "workspace": "acme",
		"tokenFile": filepath.Join(dir, "token"), "listen": "unix://" + filepath.Join(dir, socketName), "coreURL": "http://core.test",
	}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("flag %s = %v, want %v", k, r[k], v)
		}
	}
	// booth-api's own environment (its DSN, its minting credential) never reaches a sidecar.
	for _, e := range r["env"].([]any) {
		if s := e.(string); !strings.HasPrefix(s, "BOOTH_FAKE_") {
			t.Errorf("sidecar inherited %q", s)
		}
	}
	// The token file is private to booth-api's uid and holds the minted token.
	st, err := os.Stat(filepath.Join(dir, "token"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v", st, err)
	}
	if d, _ := os.Stat(dir); d.Mode().Perm() != 0o700 {
		t.Errorf("sidecar dir mode = %v", d.Mode().Perm())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "token")); string(b) != "jwt-acme-alice-1" {
		t.Errorf("token file = %q", b)
	}
}

func TestOwnerRefusal(t *testing.T) {
	h := newHarness(t, Config{RefusalTTL: 300 * time.Millisecond})
	h.minter.set(source.ErrOwnerNoAccess)
	if _, err := h.m.Pool(context.Background(), "acme", "mallory"); !errors.Is(err, source.ErrOwnerNoAccess) {
		t.Fatalf("err = %v", err)
	}
	if len(h.records(t)) != 0 {
		t.Error("a sidecar was started for an owner core refused")
	}
	// Remembered briefly: a burst of requests doesn't become a burst of mint calls.
	for range 5 {
		_, _ = h.m.Pool(context.Background(), "acme", "mallory")
	}
	if n := h.minter.count(); n != 1 {
		t.Errorf("%d mint calls during the refusal window, want 1", n)
	}
	time.Sleep(400 * time.Millisecond)
	h.minter.set(nil)
	query(t, h, "acme", "mallory")
}

func TestRefreshAndLosingAccess(t *testing.T) {
	h := newHarness(t, Config{RefusalTTL: time.Hour})
	h.minter.ttl = 600 * time.Millisecond // re-minted every ~400ms
	query(t, h, "acme", "alice")
	tokenPath := filepath.Join(h.dir, dirName(key{"acme", "alice"}), "token")
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(tokenPath)
		if string(b) != "jwt-acme-alice-1" && string(b) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the token was never renewed")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The creator loses access: the next renewal is refused and the sidecar stops.
	h.minter.set(source.ErrOwnerNoAccess)
	deadline = time.Now().Add(5 * time.Second)
	for h.m.Running() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the sidecar kept running after its owner was refused")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Teardown (after it leaves the map, so no new request can pick it up) removes the token.
	for {
		if _, err := os.Stat(filepath.Dir(tokenPath)); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stopped sidecar's directory (and token) is still there")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := h.m.Pool(context.Background(), "acme", "alice"); !errors.Is(err, source.ErrOwnerNoAccess) {
		t.Errorf("after losing access: %v", err)
	}
}

func TestIdleShutdownAndRestart(t *testing.T) {
	h := newHarness(t, Config{Idle: 300 * time.Millisecond, ReapEvery: 100 * time.Millisecond})
	query(t, h, "acme", "alice")
	deadline := time.Now().Add(5 * time.Second)
	for h.m.Running() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle sidecar never stopped")
		}
		time.Sleep(50 * time.Millisecond)
	}
	query(t, h, "acme", "alice") // starts again on demand
	if n := len(h.records(t)); n != 2 {
		t.Errorf("started %d times, want 2", n)
	}
}

func TestCrashedSidecarIsReplaced(t *testing.T) {
	h := newHarness(t, Config{})
	query(t, h, "acme", "alice")
	h.m.mu.Lock()
	e := h.m.entries[key{"acme", "alice"}]
	h.m.mu.Unlock()
	_ = e.cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for h.m.Running() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a crashed sidecar stayed registered")
		}
		time.Sleep(50 * time.Millisecond)
	}
	query(t, h, "acme", "alice")
}

func TestStartFailure(t *testing.T) {
	h := newHarness(t, Config{StartTimeout: 3 * time.Second}, "BOOTH_FAKE_FAIL=1")
	_, err := h.m.Pool(context.Background(), "acme", "alice")
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Errorf("err = %v", err)
	}
	if h.m.Running() != 0 {
		t.Error("a failed start stayed registered")
	}
}

func TestClose(t *testing.T) {
	h := newHarness(t, Config{})
	query(t, h, "acme", "alice")
	query(t, h, "globex", "bob")
	h.m.Close()
	if h.m.Running() != 0 {
		t.Errorf("%d still running", h.m.Running())
	}
	if entries, _ := os.ReadDir(h.dir); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	if _, err := h.m.Pool(context.Background(), "acme", "alice"); !errors.Is(err, source.ErrUnavailable) {
		t.Errorf("after Close: %v", err)
	}
}
