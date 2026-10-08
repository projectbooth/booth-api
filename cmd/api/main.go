// Command api is booth-api's entrypoint: generated read-only REST and GraphQL APIs over catalog
// datasets, authenticated by module-issued API keys (ADR 0100). Today it serves the management API
// (generating API definitions, issuing and revoking keys); the generated endpoints wait on core's
// public routes (ADR 0101) and the sidecar data path (ADR 0103).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-api/internal/api"
	"github.com/projectbooth/booth-api/internal/apis"
	"github.com/projectbooth/booth-api/internal/auth"
	"github.com/projectbooth/booth-api/internal/catalog"
	"github.com/projectbooth/booth-api/internal/config"
	"github.com/projectbooth/booth-api/internal/keys"
	"github.com/projectbooth/booth-api/internal/server"
	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/store"
)

const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// pgxpool connects lazily, so a database that comes up after this pod is not fatal: migrations
	// retry in the background and /healthz reports unready until they have applied.
	pool, err := pgxpool.New(ctx, cfg.DatabaseDSN)
	if err != nil {
		return fmt.Errorf("parsing database DSN: %w", err)
	}
	defer pool.Close()
	var ready atomic.Bool
	go retry(ctx, "applying migrations", 3*time.Second, func() error { return store.Migrate(ctx, pool) }, func() {
		ready.Store(true)
		log.Print("migrations applied; ready")
	})

	// The management API's verifier is built in the background, retrying OIDC discovery; until it
	// works the API answers 503 rather than letting anything through (internal/api).
	var verifier atomic.Pointer[auth.Verifier]
	if cfg.OIDC.IssuerURL == "" {
		log.Print("WARNING: BOOTH_OIDC_ISSUER_URL is not set; the management API (/api/*) will answer 503. Set oidc.issuerUrl/clientId in the chart to enable it.")
	} else {
		go retry(ctx, "OIDC discovery", 10*time.Second, func() error {
			v, err := auth.NewVerifier(ctx, cfg.OIDC)
			if err == nil {
				verifier.Store(v)
			}
			return err
		}, func() { log.Printf("management API: verifying tokens against %s", cfg.OIDC.IssuerURL) })
	}
	if cfg.CoreURL == "" {
		log.Print("WARNING: BOOTH_CORE_URL is not set; generating an API will fail, since the catalog is reached through core's gateway.")
	}

	st := store.New(pool)
	httpServer := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: server.NewRouter(server.Deps{
			DB:    pool,
			Ready: &ready,
			API: api.NewHandler(api.Deps{
				Verifier: func() auth.TokenVerifier {
					if v := verifier.Load(); v != nil {
						return v
					}
					return nil
				},
				// No data source until the sidecar path is built (ADR 0103 item 4): generating an
				// API answers 503 with a message saying so.
				APIs: apis.Service{Catalog: &catalog.Client{CoreURL: cfg.CoreURL}, Store: st, Pools: source.Unavailable{}},
				Keys: keys.Service{Store: st},
			}),
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("booth-api listening on %s", cfg.HTTPAddr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// retry runs fn until it succeeds (then calls done) or ctx ends, logging each failure.
func retry(ctx context.Context, what string, every time.Duration, fn func() error, done func()) {
	for {
		err := fn()
		if err == nil {
			done()
			return
		}
		log.Printf("%s failed (%v); retrying", what, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
