package config

import (
	"testing"
	"time"
)

func mapGetter(mapEnv map[string]string) func(string) string {
	return func(k string) string { return mapEnv[k] }
}

func TestFromEnvDefaults(t *testing.T) {
	cfg, err := FromEnv(mapGetter(map[string]string{
		"API_KEY":        "secret",
		"DEFAULT_DOMAIN": "lampa.example.com",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want :8080", cfg.Listen)
	}
	if cfg.UpstreamRepo != "https://github.com/yumata/lampa-source.git" {
		t.Errorf("UpstreamRepo = %q", cfg.UpstreamRepo)
	}
	if cfg.UpstreamBranch != "main" {
		t.Errorf("UpstreamBranch = %q", cfg.UpstreamBranch)
	}
	if cfg.PollInterval != 0 {
		t.Errorf("PollInterval = %v, want 0 (off)", cfg.PollInterval)
	}
	if cfg.DataDir != "/data" {
		t.Errorf("DataDir = %q, want /data", cfg.DataDir)
	}
	if cfg.AssetsDir != "." {
		t.Errorf("AssetsDir = %q, want .", cfg.AssetsDir)
	}
	if cfg.CacheSize != 10 {
		t.Errorf("CacheSize = %d, want 10", cfg.CacheSize)
	}
}

func TestFromEnvValid(t *testing.T) {
	cfg, err := FromEnv(mapGetter(map[string]string{
		"API_KEY":         "k",
		"DEFAULT_DOMAIN":  "d.example",
		"LISTEN":          ":9090",
		"UPSTREAM_REPO":   "https://example.com/repo.git",
		"UPSTREAM_BRANCH": "master",
		"POLL_INTERVAL":   "6h",
		"DATA_DIR":        "/tmp/x",
		"ASSETS_DIR":      "/app",
		"CACHE_SIZE":      "3",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Listen != ":9090" || cfg.UpstreamBranch != "master" || cfg.DataDir != "/tmp/x" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.PollInterval != 6*time.Hour {
		t.Errorf("PollInterval = %v, want 6h", cfg.PollInterval)
	}
	if cfg.CacheSize != 3 {
		t.Errorf("CacheSize = %d, want 3", cfg.CacheSize)
	}
}

func TestFromEnvMissingRequired(t *testing.T) {
	for _, env := range []map[string]string{
		{"DEFAULT_DOMAIN": "d.example"}, // no API_KEY
		{"API_KEY": "k"},                // no DEFAULT_DOMAIN
	} {
		if _, err := FromEnv(mapGetter(env)); err == nil {
			t.Errorf("expected error for env %v", env)
		}
	}
}

func TestFromEnvBadValues(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"bad domain":     {"API_KEY": "k", "DEFAULT_DOMAIN": "https://x:80"},
		"bad interval":   {"API_KEY": "k", "DEFAULT_DOMAIN": "d.example", "POLL_INTERVAL": "abc"},
		"neg interval":   {"API_KEY": "k", "DEFAULT_DOMAIN": "d.example", "POLL_INTERVAL": "-5m"},
		"bad cache size": {"API_KEY": "k", "DEFAULT_DOMAIN": "d.example", "CACHE_SIZE": "0"},
		"nan cache size": {"API_KEY": "k", "DEFAULT_DOMAIN": "d.example", "CACHE_SIZE": "ten"},
	} {
		if _, err := FromEnv(mapGetter(env)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestValidDomain(t *testing.T) {
	valid := []string{"lampa.example.com", "a.b", "x-y.example", "127.0.0.1", "d1.example"}
	invalid := []string{"", "UPPER.example", "a b.example", "a/b", "a:8080", "a_b.example", "http://a.example"}
	for _, d := range valid {
		if !ValidDomain(d) {
			t.Errorf("ValidDomain(%q) = false, want true", d)
		}
	}
	for _, d := range invalid {
		if ValidDomain(d) {
			t.Errorf("ValidDomain(%q) = true, want false", d)
		}
	}
}
