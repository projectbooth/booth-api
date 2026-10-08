package config

import "testing"

func TestLoad_RequiresDSN(t *testing.T) {
	t.Setenv("BOOTH_API_DATABASE_DSN", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load succeeded with no database DSN")
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("BOOTH_API_DATABASE_DSN", "postgres://u:p@h/db")
	t.Setenv("BOOTH_HTTP_ADDR", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.DatabaseDSN != "postgres://u:p@h/db" || cfg.OIDC.GroupsClaim != "groups" || cfg.OIDC.RequireAudience {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoad_DataAccess(t *testing.T) {
	t.Setenv("BOOTH_API_DATABASE_DSN", "postgres://u:p@h/db")
	cfg, err := Load()
	if err != nil || cfg.DataAccess() || cfg.SidecarBinary != "/credential-sidecar" || cfg.SidecarIdle.String() != "10m0s" {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	t.Setenv("BOOTH_WORKLOAD_MINT_URL", "http://core/api/internal/workload-tokens")
	if _, err := Load(); err == nil {
		t.Error("a mint URL without a credential was accepted")
	}
	t.Setenv("BOOTH_WORKLOAD_MINT_CREDENTIAL", "wl.api.x")
	if _, err := Load(); err == nil {
		t.Error("data access without a core URL was accepted")
	}
	t.Setenv("BOOTH_CORE_URL", "http://core")
	if cfg, err := Load(); err != nil || !cfg.DataAccess() {
		t.Errorf("configured: %+v %v", cfg, err)
	}
	t.Setenv("BOOTH_SIDECAR_IDLE", "soon")
	if _, err := Load(); err == nil {
		t.Error("a bad idle duration was accepted")
	}
}

func TestLoad_OIDC(t *testing.T) {
	t.Setenv("BOOTH_API_DATABASE_DSN", "postgres://u:p@h/db")
	t.Setenv("BOOTH_OIDC_ISSUER_URL", "https://idp.example")
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "")
	if _, err := Load(); err == nil {
		t.Error("an issuer with no client id was accepted")
	}
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "booth")
	t.Setenv("BOOTH_OIDC_REQUIRE_AUDIENCE", "maybe")
	if _, err := Load(); err == nil {
		t.Error("a non-boolean BOOTH_OIDC_REQUIRE_AUDIENCE was accepted")
	}
	t.Setenv("BOOTH_OIDC_REQUIRE_AUDIENCE", "true")
	if cfg, err := Load(); err != nil || !cfg.OIDC.RequireAudience || cfg.OIDC.ClientID != "booth" {
		t.Errorf("cfg = %+v, %v", cfg, err)
	}
}
