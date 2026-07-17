package config

import (
	"strings"
	"testing"
	"time"
)

// clearEnv blanks every env var Load reads, so tests are hermetic even on
// machines where MERAKI_API_KEY etc. are exported.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MERAKI_API_KEY", "MERAKI_ORG_ID", "MERAKI_BASE_URL", "LISTEN_ADDRESS",
		"SCRAPE_INTERVAL", "SLOW_SCRAPE_INTERVAL", "MERAKI_TIMEOUT",
		"CLIENTS_TIMESPAN", "COLLECTORS_ENABLED", "COLLECTORS_SLOW", "LOG_LEVEL",
		"WEB_CONFIG_FILE", "MERAKI_PROXY_URL",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load([]string{"--meraki.api-key=k"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebConfigFile != "" {
		t.Errorf("WebConfigFile: got %q, want empty (plain HTTP by default)", cfg.WebConfigFile)
	}
	if cfg.ListenAddress != ":9822" {
		t.Errorf("ListenAddress: got %q, want :9822", cfg.ListenAddress)
	}
	if cfg.ScrapeInterval != 120*time.Second {
		t.Errorf("ScrapeInterval: got %v, want 120s", cfg.ScrapeInterval)
	}
	if cfg.SlowInterval != 15*time.Minute {
		t.Errorf("SlowInterval: got %v, want 15m", cfg.SlowInterval)
	}
	for _, c := range strings.Split(DefaultCollectors, ",") {
		if !cfg.Collectors[c] {
			t.Errorf("collector %q should be enabled by default", c)
		}
	}
	for _, c := range []string{"vpn", "switchports", "sensors", "alerts"} {
		if cfg.Collectors[c] {
			t.Errorf("collector %q should be disabled by default", c)
		}
	}
	for _, c := range strings.Split(DefaultSlowCollectors, ",") {
		if !cfg.SlowCollectors[c] {
			t.Errorf("collector %q should be in the slow group by default", c)
		}
	}
	if cfg.BaseURL != "https://api.meraki.com/api/v1" {
		t.Errorf("BaseURL: got %q", cfg.BaseURL)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("MERAKI_API_KEY", "env-key")
	t.Setenv("SCRAPE_INTERVAL", "45s")
	t.Setenv("COLLECTORS_ENABLED", "devices,uplinks")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKey != "env-key" {
		t.Errorf("APIKey: got %q", cfg.APIKey)
	}
	if cfg.ScrapeInterval != 45*time.Second {
		t.Errorf("ScrapeInterval: got %v, want 45s", cfg.ScrapeInterval)
	}
	if len(cfg.Collectors) != 2 || !cfg.Collectors["devices"] || !cfg.Collectors["uplinks"] {
		t.Errorf("Collectors: got %v, want devices+uplinks", cfg.Collectors)
	}
}

func TestLoadFlagBeatsEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("MERAKI_API_KEY", "env-key")
	cfg, err := Load([]string{"--meraki.api-key=flag-key"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKey != "flag-key" {
		t.Errorf("APIKey: got %q, want flag to beat env", cfg.APIKey)
	}
}

func TestLoadLogLevelNormalized(t *testing.T) {
	clearEnv(t)
	cfg, err := Load([]string{"--meraki.api-key=k", "--log.level=DEBUG"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want lowercased %q", cfg.LogLevel, "debug")
	}
}

func TestLoadValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // substring of the expected error
	}{
		{"missing API key", nil, "API key is required"},
		{"interval below 30s", []string{"--meraki.api-key=k", "--scrape.interval=10s"}, "at least 30s"},
		{"slow below fast", []string{"--meraki.api-key=k", "--scrape.slow-interval=60s"}, "must be >="},
		{"unknown collector", []string{"--meraki.api-key=k", "--collectors.enabled=devices,bogus"}, `unknown collector "bogus"`},
		{"unknown slow collector", []string{"--meraki.api-key=k", "--collectors.slow=bogus"}, `unknown slow collector "bogus"`},
		{"no collectors", []string{"--meraki.api-key=k", "--collectors.enabled=,"}, "at least one collector"},
		{"unsupported proxy scheme", []string{"--meraki.api-key=k", "--meraki.proxy-url=ftp://proxy:21"}, "unsupported proxy scheme"},
		{"proxy without scheme", []string{"--meraki.api-key=k", "--meraki.proxy-url=proxy.local:3128"}, "unsupported proxy scheme"},
		{"invalid log level", []string{"--meraki.api-key=k", "--log.level=warn"}, "invalid log.level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			_, err := Load(tc.args)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}
