// Package config handles exporter configuration from flags and environment
// variables. Environment variables override flag defaults; explicit flags
// override both.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config holds all runtime configuration for the exporter.
type Config struct {
	APIKey          string        // Meraki Dashboard API key (required)
	OrgID           string        // Organization ID; auto-discovered if empty
	BaseURL         string        // Meraki API base URL
	ProxyURL        string        // proxy for Meraki API requests; empty = environment proxy settings
	ListenAddress   string        // HTTP listen address, e.g. ":9822"
	WebConfigFile   string        // exporter-toolkit web config (TLS/basic auth); empty = plain HTTP
	ScrapeInterval  time.Duration // fast-group background poll interval
	SlowInterval    time.Duration // slow-group background poll interval
	APITimeout      time.Duration // per-request HTTP timeout
	ClientsTimespan time.Duration // lookback window for client counts
	Collectors      map[string]bool
	SlowCollectors  map[string]bool // subset of Collectors polled at SlowInterval
	LogLevel        string          // "debug" or "info"
}

// Flag defaults; environment variables override them, explicit flags
// override both.
const (
	defaultScrapeInterval = 120 * time.Second
	defaultSlowInterval   = 15 * time.Minute
	defaultAPITimeout     = 30 * time.Second
)

// AllCollectors lists every collector name the exporter knows about — valid
// to opt into via COLLECTORS_ENABLED, whether or not it's on by default.
var AllCollectors = []string{
	"devices", "uplinks", "clients", "wireless", "licenses", "apiusage",
	"vpn", "switchports", "sensors", "alerts",
}

// DefaultCollectors are enabled out of the box. vpn, switchports, sensors,
// and alerts are opt-in: switchports and sensors fan out per-port/per-device
// rather than making a single O(1) org call, so existing deployments
// shouldn't see their series count or API budget change on upgrade.
var DefaultCollectors = "devices,uplinks,clients,wireless,licenses,apiusage"

// DefaultSlowCollectors are polled at the slow interval by default: their
// data changes slowly, so frequent refreshes waste Meraki API budget. Only
// takes effect for collectors that are also enabled.
var DefaultSlowCollectors = "clients,licenses,apiusage,alerts"

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDurationOr(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// parseCollectorSet parses a comma-separated collector list, validating each
// name against AllCollectors. kind appears in error messages ("collector" or
// "slow collector").
func parseCollectorSet(list, kind string) (map[string]bool, error) {
	valid := map[string]bool{}
	for _, c := range AllCollectors {
		valid[c] = true
	}
	set := map[string]bool{}
	for _, c := range strings.Split(list, ",") {
		c = strings.TrimSpace(strings.ToLower(c))
		if c == "" {
			continue
		}
		if !valid[c] {
			return nil, fmt.Errorf("unknown %s %q (valid: %s)", kind, c, strings.Join(AllCollectors, ", "))
		}
		set[c] = true
	}
	return set, nil
}

// Load parses flags and environment variables into a Config.
func Load(args []string) (*Config, error) {
	fs := flag.NewFlagSet("meraki-exporter", flag.ContinueOnError)

	var (
		apiKey = fs.String("meraki.api-key", envOr("MERAKI_API_KEY", ""),
			"Meraki Dashboard API key (env: MERAKI_API_KEY). Prefer the env var so the key does not appear in the process list.")
		orgID = fs.String("meraki.org-id", envOr("MERAKI_ORG_ID", ""),
			"Meraki organization ID; auto-discovered if the key has exactly one org (env: MERAKI_ORG_ID)")
		baseURL = fs.String("meraki.base-url", envOr("MERAKI_BASE_URL", "https://api.meraki.com/api/v1"),
			"Meraki API base URL (env: MERAKI_BASE_URL)")
		proxyURL = fs.String("meraki.proxy-url", envOr("MERAKI_PROXY_URL", ""),
			"Proxy for Meraki API requests, e.g. http://proxy.example:3128 (env: MERAKI_PROXY_URL); empty honors HTTP_PROXY/HTTPS_PROXY/NO_PROXY")
		listen = fs.String("web.listen-address", envOr("LISTEN_ADDRESS", ":9822"),
			"Address to listen on for /metrics (env: LISTEN_ADDRESS)")
		webConfig = fs.String("web.config.file", envOr("WEB_CONFIG_FILE", ""),
			"Path to exporter-toolkit web config file enabling TLS and/or basic auth; empty serves plain HTTP (env: WEB_CONFIG_FILE)")
		interval = fs.Duration("scrape.interval", envDurationOr("SCRAPE_INTERVAL", defaultScrapeInterval),
			"Fast-group background poll interval (env: SCRAPE_INTERVAL)")
		slowIntv = fs.Duration("scrape.slow-interval", envDurationOr("SLOW_SCRAPE_INTERVAL", defaultSlowInterval),
			"Slow-group background poll interval (env: SLOW_SCRAPE_INTERVAL)")
		apiTimeout = fs.Duration("meraki.timeout", envDurationOr("MERAKI_TIMEOUT", defaultAPITimeout),
			"Per-request API timeout (env: MERAKI_TIMEOUT)")
		clientsTS = fs.Duration("clients.timespan", envDurationOr("CLIENTS_TIMESPAN", time.Hour),
			"Lookback window for client counts (env: CLIENTS_TIMESPAN)")
		collectors = fs.String("collectors.enabled", envOr("COLLECTORS_ENABLED", DefaultCollectors),
			"Comma-separated collectors to enable (env: COLLECTORS_ENABLED)")
		slowNames = fs.String("collectors.slow", envOr("COLLECTORS_SLOW", DefaultSlowCollectors),
			"Comma-separated collectors polled at the slow interval (env: COLLECTORS_SLOW)")
		logLevel = fs.String("log.level", envOr("LOG_LEVEL", "info"),
			"Log level: debug or info (env: LOG_LEVEL)")
	)

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if *apiKey == "" {
		return nil, errors.New("a Meraki API key is required: set MERAKI_API_KEY or --meraki.api-key")
	}
	if *interval < 30*time.Second {
		return nil, errors.New("scrape.interval must be at least 30s to respect Meraki API rate limits")
	}
	if *proxyURL != "" {
		u, err := url.Parse(*proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid meraki.proxy-url: %w", err)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
			// schemes net/http's transport can proxy through
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q in meraki.proxy-url (use http, https, socks5, or socks5h)", u.Scheme)
		}
	}

	level := strings.ToLower(*logLevel)
	switch level {
	case "debug", "info":
	default:
		return nil, fmt.Errorf("invalid log.level %q (valid: debug, info)", *logLevel)
	}

	enabled, err := parseCollectorSet(*collectors, "collector")
	if err != nil {
		return nil, err
	}
	if len(enabled) == 0 {
		return nil, errors.New("at least one collector must be enabled")
	}

	if *slowIntv < *interval {
		return nil, errors.New("scrape.slow-interval must be >= scrape.interval")
	}
	slow, err := parseCollectorSet(*slowNames, "slow collector")
	if err != nil {
		return nil, err
	}

	return &Config{
		APIKey:          *apiKey,
		OrgID:           *orgID,
		BaseURL:         strings.TrimRight(*baseURL, "/"),
		ProxyURL:        *proxyURL,
		ListenAddress:   *listen,
		WebConfigFile:   *webConfig,
		ScrapeInterval:  *interval,
		SlowInterval:    *slowIntv,
		APITimeout:      *apiTimeout,
		ClientsTimespan: *clientsTS,
		Collectors:      enabled,
		SlowCollectors:  slow,
		LogLevel:        level,
	}, nil
}
