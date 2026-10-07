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
	if cfg.HTTPAddr != ":8080" || cfg.DatabaseDSN != "postgres://u:p@h/db" {
		t.Errorf("cfg = %+v", cfg)
	}
}
