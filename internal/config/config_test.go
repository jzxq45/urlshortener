package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := Default()
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("defaults changed: %+v", cfg)
	}
}

func TestLoadFromFile(t *testing.T) {
	// not parallel: t.Setenv is disallowed in parallel tests
	path := filepath.Join(t.TempDir(), "c.yaml")
	yaml := `listen: 127.0.0.1:9090
db_path: data/test.db
base_url: https://s.example.com
retention_days: 30
rate_limit_per_min: 5
log_level: debug
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9090" || cfg.BaseURL != "https://s.example.com" {
		t.Errorf("fields overridden: %+v", cfg)
	}
	// an env var overrides the file
	t.Setenv("SHORTENER_BASE_URL", "https://env.example.com")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("load env: %v", err)
	}
	if cfg.BaseURL != "https://env.example.com" {
		t.Errorf("env override ignored: %+v", cfg)
	}
}

func TestValidateFailures(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Config){
		"listen":             func(c *Config) { c.Listen = "no-port" },
		"db_path":            func(c *Config) { c.DBPath = "" },
		"base_url":           func(c *Config) { c.BaseURL = "ftp://x" },
		"trailing /":         func(c *Config) { c.BaseURL = "http://x/" },
		"retention":          func(c *Config) { c.RetentionDays = 0 },
		"rate limit":         func(c *Config) { c.RateLimitPerMin = -1 },
		"log level":          func(c *Config) { c.LogLevel = "verbose" },
		"trusted proxie cid": func(c *Config) { c.TrustedProxies = []string{"not-an-ip"} },
	}
	for name, mutate := range cases {
		cfg := Default()
		mutate(&cfg)
		if err := cfg.validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestLoadMissingFileIsError(t *testing.T) {
	t.Parallel()
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("missing file must be an error")
	}
}
