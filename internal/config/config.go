// Package config loads booth-api's runtime configuration from environment variables. Every value
// maps 1:1 to a Helm chart value/env var, mirroring the sibling modules' own internal/config —
// there is no config file format of our own to version.
package config

import (
	"fmt"
	"os"
)

// Config is booth-api's full runtime configuration. Scaffold-sized on purpose: the API-key path
// and the data-access layer (ADR 0100) add their settings here when they are built, not before.
type Config struct {
	HTTPAddr string

	// DatabaseDSN is this module's OWN database (API keys and generated-API definitions), the one
	// booth-core provisions because the manifest declares `database: {enabled: true}` (ADR 0053)
	// and delivers as the booth-database-credentials Secret's `dsn` key. It is not a workspace's
	// booth-database (ADR 0081), which generated APIs read through the credential sidecar
	// (ADR 0095) and which is not wired in yet.
	DatabaseDSN string
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:    getEnv("BOOTH_HTTP_ADDR", ":8080"),
		DatabaseDSN: os.Getenv("BOOTH_API_DATABASE_DSN"),
	}
	if cfg.DatabaseDSN == "" {
		return Config{}, fmt.Errorf("BOOTH_API_DATABASE_DSN is required (the chart sets it from booth-core's booth-database-credentials Secret)")
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
