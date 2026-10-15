// Command api is booth-api's entrypoint: generated REST and GraphQL APIs over catalog datasets,
// authenticated by module-issued API keys (ADR 0100): the management API at /api/, the
// key-authenticated generated endpoints at /v1/ (core's public route, ADR 0101), and app writes at
// /app/v1/ (ADR 0117), authenticated by an app's workload token.
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
	"github.com/projectbooth/booth-api/internal/appauth"
	"github.com/projectbooth/booth-api/internal/appwrite"
	"github.com/projectbooth/booth-api/internal/auth"
	"github.com/projectbooth/booth-api/internal/broker"
	"github.com/projectbooth/booth-api/internal/catalog"
	"github.com/projectbooth/booth-api/internal/config"
	"github.com/projectbooth/booth-api/internal/gql"
	"github.com/projectbooth/booth-api/internal/keys"
	"github.com/projectbooth/booth-api/internal/public"
	"github.com/projectbooth/booth-api/internal/ratelimit"
	"github.com/projectbooth/booth-api/internal/server"
	"github.com/projectbooth/booth-api/internal/sidecars"
	"github.com/projectbooth/booth-api/internal/source"
	"github.com/projectbooth/booth-api/internal/store"
	"github.com/projectbooth/booth-api/internal/table"
	"github.com/projectbooth/booth-api/internal/workload"
	"github.com/projectbooth/booth-api/internal/writes"
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
		}, func() {}) // auth.NewVerifier logs the issuer and where keys come from once, on success
	}
	if cfg.CoreURL == "" {
		log.Print("WARNING: BOOTH_CORE_URL is not set; generating an API will fail, since the catalog is reached through core's gateway.")
	}

	st := store.New(pool)
	// Workspace data (ADR 0103): one credential-sidecar process per workspace and key creator,
	// under a workload token core mints for that creator. Without the minting Secret, data requests
	// and API generation answer 503; keys, scope and the schema documents still work.
	var pools source.Pools = source.Unavailable{}
	// Writes (ADR 0117): the same manager runs one write sidecar per (API, granting owner), and
	// booth-api calls the broker itself to keep the write group in step with each API's settings.
	ws := writes.Service{
		Store: st, Limiter: ratelimit.New(cfg.WriteRate, cfg.WriteBurst),
		Timeout: table.DefaultLimits.StatementTimeout, IdempotencyTTL: writes.DefaultIdempotencyTTL,
	}
	if cfg.DataAccess() {
		minter := &workload.Minter{URL: cfg.MintURL, Credential: cfg.MintCredential}
		mgr, err := sidecars.New(sidecars.Config{
			Binary: cfg.SidecarBinary, Dir: cfg.SidecarDir, CoreURL: cfg.CoreURL, Idle: cfg.SidecarIdle,
		}, minter)
		if err != nil {
			return fmt.Errorf("starting the sidecar manager: %w", err)
		}
		defer mgr.Close()
		pools = mgr
		ws.Pools, ws.Minter, ws.Broker = mgr, minter, &broker.Client{CoreURL: cfg.CoreURL}
		log.Printf("data access: credential sidecars from %s, idle shutdown after %s", cfg.SidecarBinary, cfg.SidecarIdle)
	} else {
		log.Print("WARNING: no workload minting credential; reading workspace data is off (data requests answer 503)")
	}

	// App tokens on /app/v1/ are verified against core's workload issuer, discovered in the
	// background like the OIDC issuer; until it answers the app routes answer 503.
	var appVerifier atomic.Pointer[appauth.Verifier]
	if cfg.WorkloadIssuerURL == "" {
		log.Print("WARNING: BOOTH_OIDC_WORKLOAD_ISSUER_URL is not set; app writes (/app/v1/*) will answer 503. Set oidc.workloadIssuerUrl in the chart to enable them.")
	} else {
		go retry(ctx, "workload issuer discovery", 10*time.Second, func() error {
			v, err := appauth.NewVerifier(ctx, appauth.Config{
				IssuerURL: cfg.WorkloadIssuerURL, ClientID: cfg.OIDC.ClientID, RequireAudience: cfg.OIDC.RequireAudience, GroupsClaim: cfg.OIDC.GroupsClaim,
			})
			if err == nil {
				appVerifier.Store(v)
			}
			return err
		}, func() {})
	}
	// The write audit log and idempotency keys are pruned hourly.
	go func() {
		for {
			if n, err := st.Prune(ctx, cfg.AuditRetention, writes.DefaultIdempotencyTTL); err != nil {
				if ctx.Err() == nil {
					log.Printf("writes: pruning the audit log: %v", err)
				}
			} else if n > 0 {
				log.Printf("writes: pruned %d audit and idempotency records", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()
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
				APIs:   apis.Service{Catalog: &catalog.Client{CoreURL: cfg.CoreURL}, Store: st, Pools: pools},
				Keys:   keys.Service{Store: st},
				Writes: ws,
			}),
			Public: public.NewHandler(public.Deps{Keys: keys.Service{Store: st}, APIs: st, Pools: pools, Limits: gql.DefaultLimits}),
			App: appwrite.NewHandler(appwrite.Deps{
				Verifier: func() appauth.TokenVerifier {
					if v := appVerifier.Load(); v != nil {
						return v
					}
					return nil
				},
				Writes: ws,
				Limits: gql.DefaultLimits,
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
