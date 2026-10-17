// Package config loads service configuration from YAML with env overrides.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the service configuration. Every field has a default so the
// server can start with an empty config file.
type Config struct {
	Listen          string   `yaml:"listen"`
	DBPath          string   `yaml:"db_path"`
	BaseURL         string   `yaml:"base_url"`
	RetentionDays   int      `yaml:"retention_days"`
	RateLimitPerMin int      `yaml:"rate_limit_per_min"`
	LogLevel        string   `yaml:"log_level"`
	TrustedProxies  []string `yaml:"trusted_proxies"` // IP or CIDR of reverse proxies
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Listen:          "0.0.0.0:8080",
		DBPath:          "urlshortener.db",
		BaseURL:         "http://localhost:8080",
		RetentionDays:   90,
		RateLimitPerMin: 10,
		LogLevel:        "info",
		// empty on purpose: without a configured proxy the client IP must
		// come from the socket, not from spoofable request headers
		TrustedProxies: nil,
	}
}

// Load reads YAML from path (optional), applies SHORTENER_* env overrides,
// and validates the result. A missing file is an error; pass an empty path
// to skip the file entirely and use defaults+env.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("config: %w", err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	cfg.applyEnv()
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("SHORTENER_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("SHORTENER_DB_PATH"); v != "" {
		c.DBPath = v
	}
	if v := os.Getenv("SHORTENER_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("SHORTENER_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.RetentionDays = n
		}
	}
	if v := os.Getenv("SHORTENER_RATE_LIMIT_PER_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.RateLimitPerMin = n
		}
	}
	if v := os.Getenv("SHORTENER_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("SHORTENER_TRUSTED_PROXIES"); v != "" {
		// env arrives as a comma-separated list (lists can't ride ENV arrays)
		parts := strings.Split(v, ",")
		c.TrustedProxies = c.TrustedProxies[:0]
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				c.TrustedProxies = append(c.TrustedProxies, p)
			}
		}
	}
}

func (c Config) validate() error {
	if !strings.Contains(c.Listen, ":") {
		return fmt.Errorf("config: listen %q must be host:port", c.Listen)
	}
	if c.DBPath == "" {
		return fmt.Errorf("config: db_path is empty")
	}
	if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return fmt.Errorf("config: base_url must start with http(s)://, got %q", c.BaseURL)
	}
	if strings.HasSuffix(c.BaseURL, "/") {
		return fmt.Errorf("config: base_url must not end with /")
	}
	if c.RetentionDays <= 0 {
		return fmt.Errorf("config: retention_days must be > 0, got %d", c.RetentionDays)
	}
	if c.RateLimitPerMin <= 0 {
		return fmt.Errorf("config: rate_limit_per_min must be > 0, got %d", c.RateLimitPerMin)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: log_level must be debug|info|warn|error, got %q", c.LogLevel)
	}
	for _, tp := range c.TrustedProxies {
		if ip := net.ParseIP(tp); ip != nil {
			continue
		}
		if _, _, err := net.ParseCIDR(tp); err != nil {
			return fmt.Errorf("config: trusted_proxies entry %q is not an IP or CIDR", tp)
		}
	}
	return nil
}
