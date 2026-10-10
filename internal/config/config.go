// Package config loads the builder configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the runtime configuration, sourced from environment variables.
type Config struct {
	Listen         string
	APIKey         string
	UpstreamRepo   string
	UpstreamBranch string
	DefaultDomain  string
	PollInterval   time.Duration // 0 means polling is disabled
	DataDir        string
	AssetsDir      string // patches/, overlay/ and package-lock.json live here
	CacheSize      int    // raw configured value; the effective value is
	// Builder.cacheSize in internal/builder, capped at state.MaxHistory
}

// ValidDomain reports whether s is an acceptable domain: lowercase letters,
// digits, dots and dashes only — no scheme, path or port. Extend the accepted
// syntax deliberately, not casually.
func ValidDomain(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

// FromOS builds a Config from the process environment.
func FromOS() (Config, error) {
	return FromEnv(os.Getenv)
}

// FromEnv builds a Config from the given getter. An empty value is treated
// as unset. Missing required variables and malformed values are reported as
// errors (fatal for the caller).
func FromEnv(get func(string) string) (Config, error) {
	cfg := Config{
		Listen:         envOr(get, "LISTEN", ":8080"),
		APIKey:         get("API_KEY"),
		UpstreamRepo:   envOr(get, "UPSTREAM_REPO", "https://github.com/yumata/lampa-source.git"),
		UpstreamBranch: envOr(get, "UPSTREAM_BRANCH", "main"),
		DefaultDomain:  get("DEFAULT_DOMAIN"),
		DataDir:        envOr(get, "DATA_DIR", "/data"),
		AssetsDir:      envOr(get, "ASSETS_DIR", "."),
	}
	if cfg.APIKey == "" {
		return Config{}, errors.New("API_KEY is required")
	}
	if cfg.DefaultDomain == "" {
		return Config{}, errors.New("DEFAULT_DOMAIN is required")
	}
	if !ValidDomain(cfg.DefaultDomain) {
		return Config{}, fmt.Errorf("DEFAULT_DOMAIN %q must match ^[a-z0-9.-]+$", cfg.DefaultDomain)
	}
	raw := envOr(get, "POLL_INTERVAL", "0")
	if raw != "0" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("POLL_INTERVAL %q: want a positive duration like 10m or 6h, or 0", raw)
		}
		cfg.PollInterval = d
	}
	raw = envOr(get, "CACHE_SIZE", "10")
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return Config{}, fmt.Errorf("CACHE_SIZE %q: want an integer >= 1", raw)
	}
	cfg.CacheSize = n
	return cfg, nil
}

func envOr(get func(string) string, key, def string) string {
	if v := get(key); v != "" {
		return v
	}
	return def
}
