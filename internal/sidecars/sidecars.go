// Package sidecars runs booth-core's credential sidecar (ADR 0095, contracts/credential-sidecar.md)
// as child processes of booth-api, one per workspace and key creator, in the contract's "one
// process per identity" shape (its "Deployment granularity" section), and hands out connection
// pools on them (ADR 0103).
//
// Per (workspace, owner):
//   - a workload token is minted with that owner (internal/workload) and written, 0600, to a file
//     in a private 0700 directory; it is re-minted before it expires, and a refusal (the owner lost
//     access or hasn't signed in recently) stops the sidecar, so a key stops working within one
//     token lifetime of its creator losing access;
//   - `credential-sidecar --kind=postgres --access=read` runs with `--token-file` pointing at that
//     file and `--listen` on a Unix socket in the same directory, so no other process in the pod
//     can share its identity by port and booth-api never sees a database password;
//   - a pgx pool connects to the socket, with connection lifetimes kept well inside the sidecar's
//     guaranteed minimum (about half a lease; the contract's "Connection lifetime");
//   - after Idle with no request the pool is closed, the process stopped and the directory removed.
//
// Why per (workspace, owner) and not per workspace: ADR 0103 bounds each key by its own creator's
// access ("a key stops working a week after its creator last signs in"). A single token per
// workspace would carry one person's access for every key there, so a key whose creator had lost
// access would keep working on someone else's token. The lease is the same either way (read on the
// workspace database), so this costs only extra processes, which the idle shutdown bounds.
package sidecars

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/workload"
)

// Minter mints workload tokens (internal/workload.Minter).
type Minter interface {
	Mint(ctx context.Context, workspace, owner string) (workload.Token, error)
}

// Config configures the manager. Zero durations take the defaults below.
type Config struct {
	// Binary is the credential-sidecar executable, copied into booth-api's image from the
	// digest-pinned ghcr.io/projectbooth/credential-sidecar image (Dockerfile).
	Binary string
	// Dir holds one private directory per running sidecar (an emptyDir in the chart).
	Dir     string
	CoreURL string
	// Idle stops a sidecar no request has used for this long. Default 10 minutes.
	Idle time.Duration
	// StartTimeout bounds minting, starting and the first healthy /healthz. Default 30 seconds.
	StartTimeout time.Duration
	// MaxConns per pool. Default 4.
	MaxConns int32
	// ConnLifetime recycles connections. Default 15 minutes: the sidecar guarantees a connection
	// at least about half a lease (30 minutes with booth-database's one-hour floor).
	ConnLifetime time.Duration
	// RefusalTTL remembers an owner refusal so a burst of requests on a dead key doesn't turn into
	// a burst of mint calls. Default 30 seconds.
	RefusalTTL time.Duration
	// ReapEvery is how often idle sidecars are looked for. Default 1 minute.
	ReapEvery time.Duration
	// ConnString builds the pool's connection string for a socket directory. Default:
	// `host=<dir> port=5432 ... sslmode=disable`; the sidecar ignores the user and database a
	// client names and uses its lease's own. Tests override it.
	ConnString func(dir string) string
	// ExtraEnv is added to the sidecar's otherwise empty environment (tests only). The sidecar
	// never inherits booth-api's environment, which holds booth-api's own database DSN and its
	// minting credential.
	ExtraEnv []string
}

const socketName = ".s.PGSQL.5432" // what libpq/pgx dial for host=<dir> port=5432

func (c *Config) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	set(&c.Idle, 10*time.Minute)
	set(&c.StartTimeout, 30*time.Second)
	set(&c.ConnLifetime, 15*time.Minute)
	set(&c.RefusalTTL, 30*time.Second)
	set(&c.ReapEvery, time.Minute)
	if c.MaxConns <= 0 {
		c.MaxConns = 4
	}
	if c.ConnString == nil {
		c.ConnString = func(dir string) string {
			return fmt.Sprintf("host=%s port=5432 user=booth_api dbname=workspace sslmode=disable", dir)
		}
	}
}

type key struct{ workspace, owner string }

type entry struct {
	k        key
	dir      string
	ready    chan struct{}
	err      error
	pool     *pgxpool.Pool
	cmd      *exec.Cmd
	exited   chan struct{}
	lastUsed atomic.Int64
	stopping atomic.Bool
	cancel   context.CancelFunc
	stopOnce sync.Once
}

// Manager implements source.Pools with sidecar processes.
type Manager struct {
	cfg     Config
	minter  Minter
	mu      sync.Mutex
	entries map[key]*entry
	refused map[key]time.Time
	closed  bool
	stopBg  context.CancelFunc
	now     func() time.Time
}

// New starts a manager. Close stops every sidecar it started.
func New(cfg Config, m Minter) (*Manager, error) {
	cfg.defaults()
	if cfg.Binary == "" || cfg.Dir == "" || cfg.CoreURL == "" {
		return nil, errors.New("sidecars: Binary, Dir and CoreURL are required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr := &Manager{cfg: cfg, minter: m, entries: map[key]*entry{}, refused: map[key]time.Time{}, stopBg: cancel, now: time.Now}
	go mgr.reap(ctx)
	return mgr, nil
}

var _ source.Pools = (*Manager)(nil)

// Pool returns a pool on workspace's database under owner's workload identity, starting a sidecar
// if none is running for that pair.
func (m *Manager) Pool(ctx context.Context, workspace, owner string) (*pgxpool.Pool, error) {
	k := key{workspace, owner}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, source.ErrUnavailable
	}
	if until, ok := m.refused[k]; ok {
		if m.now().Before(until) {
			m.mu.Unlock()
			return nil, source.ErrOwnerNoAccess
		}
		delete(m.refused, k)
	}
	e := m.entries[k]
	if e == nil {
		e = &entry{k: k, ready: make(chan struct{}), dir: filepath.Join(m.cfg.Dir, dirName(k))}
		e.lastUsed.Store(m.now().UnixNano())
		m.entries[k] = e
		go m.start(e)
	}
	m.mu.Unlock()

	select {
	case <-e.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if e.err != nil {
		return nil, e.err
	}
	e.lastUsed.Store(m.now().UnixNano())
	return e.pool, nil
}

// Running reports how many sidecars are up (for tests and logs).
func (m *Manager) Running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// Close stops every sidecar and refuses further requests.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	all := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		all = append(all, e)
	}
	m.entries = map[key]*entry{}
	m.mu.Unlock()
	m.stopBg()
	for _, e := range all {
		<-e.ready
		m.stop(e, "booth-api shutting down")
	}
}

func dirName(k key) string {
	sum := sha256.Sum256([]byte(k.workspace + "\x00" + k.owner))
	return hex.EncodeToString(sum[:8])
}

func (m *Manager) start(e *entry) {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	startCtx, startCancel := context.WithTimeout(ctx, m.cfg.StartTimeout)
	defer startCancel()

	fail := func(err error) {
		if errors.Is(err, source.ErrOwnerNoAccess) {
			m.mu.Lock()
			m.refused[e.k] = m.now().Add(m.cfg.RefusalTTL)
			m.mu.Unlock()
		} else {
			log.Printf("sidecars: starting for workspace %s: %v", e.k.workspace, err)
		}
		e.err = err
		m.forget(e)
		close(e.ready)
		m.stop(e, "start failed")
	}

	_ = os.RemoveAll(e.dir)
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		fail(err)
		return
	}
	tok, err := m.minter.Mint(startCtx, e.k.workspace, e.k.owner)
	if err != nil {
		fail(err)
		return
	}
	if err := writeToken(e.dir, tok.JWT); err != nil {
		fail(err)
		return
	}

	scope, _ := json.Marshal(map[string]string{"workspace": e.k.workspace})
	cmd := exec.Command(m.cfg.Binary,
		"--kind=postgres",
		"--access=read",
		"--scope="+string(scope),
		"--workspace="+e.k.workspace,
		"--token-file="+filepath.Join(e.dir, "token"),
		"--listen=unix://"+filepath.Join(e.dir, socketName),
		"--core-url="+m.cfg.CoreURL,
	)
	cmd.Env = append([]string{}, m.cfg.ExtraEnv...)
	logs := logWriter("sidecar[" + e.k.workspace + "]")
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		fail(fmt.Errorf("starting the credential sidecar: %w", err))
		return
	}
	e.cmd = cmd
	e.exited = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = logs.Close()
		close(e.exited)
		if !e.stopping.Load() {
			log.Printf("sidecars: the sidecar for workspace %s exited unexpectedly; the next request starts a new one", e.k.workspace)
			m.forget(e)
			m.stop(e, "exited")
		}
	}()

	if err := waitHealthy(startCtx, filepath.Join(e.dir, socketName), e.exited); err != nil {
		fail(err)
		return
	}
	cfg, err := pgxpool.ParseConfig(m.cfg.ConnString(e.dir))
	if err != nil {
		fail(err)
		return
	}
	cfg.MaxConns = m.cfg.MaxConns
	cfg.MaxConnLifetime = m.cfg.ConnLifetime
	cfg.MaxConnLifetimeJitter = m.cfg.ConnLifetime / 10
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err == nil {
		err = pool.Ping(startCtx)
	}
	if err != nil {
		if pool != nil {
			pool.Close()
		}
		fail(fmt.Errorf("connecting through the credential sidecar: %w", err))
		return
	}
	e.pool = pool
	close(e.ready)
	log.Printf("sidecars: started for workspace %s (owner %s)", e.k.workspace, e.k.owner)
	go m.refresh(ctx, e, m.now(), tok.ExpiresAt)
}

// refresh re-mints the token once two thirds of its life has passed. A refusal stops the sidecar
// at once; a transient failure is retried until the current token expires.
func (m *Manager) refresh(ctx context.Context, e *entry, issued, expires time.Time) {
	for {
		due := issued.Add(expires.Sub(issued) * 2 / 3)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(due)):
		}
		for {
			tok, err := m.minter.Mint(ctx, e.k.workspace, e.k.owner)
			if err == nil {
				if err := writeToken(e.dir, tok.JWT); err != nil {
					log.Printf("sidecars: writing the renewed token for workspace %s: %v", e.k.workspace, err)
				}
				issued, expires = m.now(), tok.ExpiresAt
				break
			}
			if errors.Is(err, source.ErrOwnerNoAccess) {
				m.mu.Lock()
				m.refused[e.k] = m.now().Add(m.cfg.RefusalTTL)
				m.mu.Unlock()
				m.forget(e)
				m.stop(e, "owner no longer has access")
				return
			}
			if ctx.Err() != nil {
				return
			}
			if !m.now().Before(expires) {
				log.Printf("sidecars: couldn't renew the token for workspace %s before it expired: %v", e.k.workspace, err)
				m.forget(e)
				m.stop(e, "token expired")
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(minDuration(15*time.Second, time.Until(expires))):
			}
		}
	}
}

func (m *Manager) reap(ctx context.Context) {
	t := time.NewTicker(m.cfg.ReapEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cutoff := m.now().Add(-m.cfg.Idle).UnixNano()
		var idle []*entry
		m.mu.Lock()
		for k, e := range m.entries {
			select {
			case <-e.ready:
			default:
				continue // still starting
			}
			if e.err == nil && e.lastUsed.Load() < cutoff {
				delete(m.entries, k)
				idle = append(idle, e)
			}
		}
		m.mu.Unlock()
		for _, e := range idle {
			m.stop(e, "idle")
		}
	}
}

// forget removes e from the map if it is still the entry for its key.
func (m *Manager) forget(e *entry) {
	m.mu.Lock()
	if m.entries[e.k] == e {
		delete(m.entries, e.k)
	}
	m.mu.Unlock()
}

// stop tears an entry down: pool, process, directory (token file included).
func (m *Manager) stop(e *entry, why string) {
	e.stopOnce.Do(func() {
		e.stopping.Store(true)
		if e.cancel != nil {
			e.cancel()
		}
		if e.pool != nil {
			e.pool.Close()
		}
		if e.cmd != nil && e.cmd.Process != nil {
			if runtime.GOOS == "windows" {
				_ = e.cmd.Process.Kill()
			} else {
				_ = e.cmd.Process.Signal(os.Interrupt)
			}
			select {
			case <-e.exited:
			case <-time.After(5 * time.Second):
				_ = e.cmd.Process.Kill()
				<-e.exited
			}
		}
		_ = os.RemoveAll(e.dir)
		if why != "start failed" {
			log.Printf("sidecars: stopped for workspace %s (%s)", e.k.workspace, why)
		}
	})
}

// writeToken replaces the token file atomically, 0600: the sidecar re-reads it on every broker
// call (FileToken), so a reader never sees a partial token.
func writeToken(dir, jwt string) error {
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(jwt); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "token"))
}

// waitHealthy polls the sidecar's /healthz (served on its own socket, contracts/credential-sidecar.md
// "Health") until it reports a lease, the process exits, or ctx ends.
func waitHealthy(ctx context.Context, socket string, exited <-chan struct{}) error {
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}
	defer client.CloseIdleConnections()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://sidecar/healthz", nil)
		if resp, err := client.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-exited:
			return errors.New("the credential sidecar exited before it obtained a lease (see its log lines)")
		case <-ctx.Done():
			return fmt.Errorf("the credential sidecar didn't report a lease in time: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// logWriter prefixes each line the sidecar writes. The sidecar never logs a credential value
// (contracts/credential-sidecar.md, "Failure behavior").
func logWriter(prefix string) *io.PipeWriter {
	r, w := io.Pipe()
	go func() {
		s := bufio.NewScanner(r)
		for s.Scan() {
			log.Printf("%s: %s", prefix, s.Text())
		}
	}()
	return w
}
