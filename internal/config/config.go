// Package config loads booth-api's runtime configuration from environment variables. Every value
// maps 1:1 to a Helm chart value/env var, mirroring the sibling modules' own internal/config —
// there is no config file format of our own to version.
package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/projectbooth/booth-api/internal/auth"
)

// Config is booth-api's full runtime configuration.
type Config struct {
	HTTPAddr string

	// DatabaseDSN is this module's OWN database (API keys and generated-API definitions), the one
	// booth-core provisions because the manifest declares `database: {enabled: true}` (ADR 0053)
	// and delivers as the booth-database-credentials Secret's `dsn` key. It is not a workspace's
	// booth-database (ADR 0081): generated APIs read those through the credential sidecar
	// (ADR 0103), which is not wired in yet.
	DatabaseDSN string

	// CoreURL is booth-core's base URL. booth-api reaches booth-catalog through core's gateway at
	// CoreURL/modules/catalog/ with the caller's own token (ADR 0007). Empty means generating an
	// API fails with a clear error; key management still works.
	CoreURL string

	// OIDC verifies callers of the management API (ADR 0041). An empty IssuerURL leaves the API
	// answering 503.
	OIDC auth.OIDCConfig
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:    getEnv("BOOTH_HTTP_ADDR", ":8080"),
		DatabaseDSN: os.Getenv("BOOTH_API_DATABASE_DSN"),
		CoreURL:     os.Getenv("BOOTH_CORE_URL"),
		OIDC: auth.OIDCConfig{
			IssuerURL:   os.Getenv("BOOTH_OIDC_ISSUER_URL"),
			ClientID:    os.Getenv("BOOTH_OIDC_CLIENT_ID"),
			GroupsClaim: getEnv("BOOTH_OIDC_GROUPS_CLAIM", auth.DefaultGroupsClaim),
		},
	}
	if cfg.DatabaseDSN == "" {
		return Config{}, fmt.Errorf("BOOTH_API_DATABASE_DSN is required (the chart sets it from booth-core's booth-database-credentials Secret)")
	}
	if v := os.Getenv("BOOTH_OIDC_REQUIRE_AUDIENCE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("BOOTH_OIDC_REQUIRE_AUDIENCE: %w", err)
		}
		cfg.OIDC.RequireAudience = b
	}
	if cfg.OIDC.IssuerURL != "" && cfg.OIDC.ClientID == "" {
		return Config{}, fmt.Errorf("BOOTH_OIDC_CLIENT_ID is required when BOOTH_OIDC_ISSUER_URL is set")
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
