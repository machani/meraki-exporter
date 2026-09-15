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

// TestLoadRejectsMalformedDurationEnv pins that a typo in a duration env var
// fails the exporter at startup instead of silently reverting to the built-in
// default. That silence mattered: the deployment profiles pin specific
// intervals and the alert staleness thresholds are tuned to them, so a quiet
// fallback would mistune alerting with no visible symptom.
func TestLoadRejectsMalformedDurationEnv(t *testing.T) {
	for _, key := range []string{"SCRAPE_INTERVAL", "SLOW_SCRAPE_INTERVAL", "MERAKI_TIMEOUT", "CLIENTS_TIMESPAN"} {
		t.Run(key, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("MERAKI_API_KEY", "k")
			t.Setenv(key, "5min") // a plausible typo for "5m"
			_, err := Load(nil)
			if err == nil {
				t.Fatalf("%s=5min: got no error, want one (a typo must not silently use the default)", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error should name the offending variable, got: %v", err)
			}
		})
	}
}

// TestLoadReportsEveryMalformedDuration checks the errors are joined rather
// than surfaced one run at a time.
func TestLoadReportsEveryMalformedDuration(t *testing.T) {
	clearEnv(t)
	t.Setenv("MERAKI_API_KEY", "k")
	t.Setenv("SCRAPE_INTERVAL", "5min")
	t.Setenv("MERAKI_TIMEOUT", "thirty")
	_, err := Load(nil)
	if err == nil {
		t.Fatal("got no error, want one")
	}
	for _, key := range []string{"SCRAPE_INTERVAL", "MERAKI_TIMEOUT"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should mention %s, got: %v", key, err)
		}
	}
}

// TestLoadAcceptsValidDurationEnv guards the obvious over-correction: a
// well-formed value must still be honoured.
func TestLoadAcceptsValidDurationEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("MERAKI_API_KEY", "k")
	t.Setenv("SCRAPE_INTERVAL", "5m")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ScrapeInterval != 5*time.Minute {
		t.Errorf("ScrapeInterval: got %v, want 5m", cfg.ScrapeInterval)
	}
}

// TestDefaultSlowCollectorsIncludeExpensiveOptIns pins that the two collectors
// that fan out per port and per device land in the slow poll group by default.
// They are the only ones that do not make a single org-wide call, so polling
// them on the fast interval would dominate the API budget of anyone who
// enables them.
func TestDefaultSlowCollectorsIncludeExpensiveOptIns(t *testing.T) {
	clearEnv(t)
	t.Setenv("MERAKI_API_KEY", "k")
	t.Setenv("COLLECTORS_ENABLED", "devices,switchports,sensors")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, c := range []string{"switchports", "sensors"} {
		if !cfg.SlowCollectors[c] {
			t.Errorf("%s should default to the slow poll group", c)
		}
	}
	if cfg.SlowCollectors["devices"] {
		t.Error("devices must stay in the fast group: it is one org-wide call and drives the status alerts")
	}
}

// TestCollectorListToleratesSpaces pins that a spaced list parses identically
// to a compact one. Deployed env files keep acquiring spaces after the commas,
// and the exporter has always coped because parseCollectorSet trims each
// element — but that was incidental, not a promise. It is a promise now.
//
// Note this tolerance does NOT extend to shells: `set -a; . env-file` chokes on
// a spaced value, so the env examples still tell operators to write them
// compact.
func TestCollectorListToleratesSpaces(t *testing.T) {
	clearEnv(t)
	t.Setenv("MERAKI_API_KEY", "k")
	t.Setenv("COLLECTORS_ENABLED", "devices, uplinks , clients")
	t.Setenv("COLLECTORS_SLOW", " clients ,  licenses ")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, c := range []string{"devices", "uplinks", "clients"} {
		if !cfg.Collectors[c] {
			t.Errorf("collector %q should be enabled despite surrounding spaces", c)
		}
	}
	if len(cfg.Collectors) != 3 {
		t.Errorf("enabled collectors: got %d, want 3 — spaces must not create extra entries", len(cfg.Collectors))
	}
	if !cfg.SlowCollectors["clients"] || !cfg.SlowCollectors["licenses"] {
		t.Errorf("slow list should tolerate spaces too, got %v", cfg.SlowCollectors)
	}
}

// TestCollectorWarnings covers the two ways COLLECTORS_SLOW quietly does
// something other than intended. Both configurations are legal, so these are
// warnings rather than errors — a legitimate deployed config would fail to
// start otherwise, and it is correct.
func TestCollectorWarnings(t *testing.T) {
	cases := []struct {
		name       string
		enabled    string
		slow       string
		wantSubstr []string
		wantNone   bool
	}{
		{
			name:       "slow names a collector that is not enabled",
			enabled:    "devices,uplinks",
			slow:       "devices,sensors",
			wantSubstr: []string{"sensors", "no effect"},
		},
		{
			name:    "replacing the default demotes an expensive collector to fast",
			enabled: "devices,uplinks,clients,switchports",
			slow:    "switchports",
			// clients is enabled, absent from the slow list, and makes one API
			// call per network — the case worth shouting about.
			wantSubstr: []string{"clients", "FAST"},
		},
		{
			name:     "defaults produce no warnings",
			enabled:  "devices,uplinks,clients,wireless,licenses,apiusage",
			slow:     "clients,licenses,apiusage,alerts,switchports,sensors",
			wantNone: true,
		},
		{
			// Deployed env files keep acquiring spaces after the commas. A
			// value that differs from the default only by whitespace is still
			// the default, so it must not warn — this is why the check
			// compares parsed sets rather than raw strings.
			name:     "the default list retyped with spaces is still the default",
			enabled:  "devices,uplinks,clients,wireless,licenses,apiusage",
			slow:     "clients, licenses, apiusage, alerts, switchports, sensors",
			wantNone: true,
		},
		{
			// A profile that deliberately keeps alerts in the fast group.
			// alerts is cheap (one org call), so this must stay silent or the
			// warning becomes noise on a correct configuration.
			name:     "deliberately moving a cheap collector to fast is silent",
			enabled:  "devices,uplinks,clients,wireless,licenses,apiusage,vpn,switchports,alerts",
			slow:     "clients,licenses,apiusage,switchports",
			wantNone: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("MERAKI_API_KEY", "k")
			t.Setenv("COLLECTORS_ENABLED", tc.enabled)
			t.Setenv("COLLECTORS_SLOW", tc.slow)
			cfg, err := Load(nil)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			joined := strings.Join(cfg.Warnings, " | ")
			if tc.wantNone {
				if len(cfg.Warnings) != 0 {
					t.Errorf("want no warnings, got: %s", joined)
				}
				return
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(joined, want) {
					t.Errorf("warning should mention %q, got: %s", want, joined)
				}
			}
		})
	}
}
