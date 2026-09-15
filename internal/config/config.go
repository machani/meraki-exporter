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
	"sort"
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

	// Warnings holds non-fatal configuration observations for the caller to
	// log at startup. Load cannot log them itself — it runs before a logger
	// exists, because the log level is one of the things it parses.
	Warnings []string
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

// fanOutCollectors make one API call per network, port or device instead of a
// single org-wide call. They are the only ones whose poll group materially
// changes API usage, which is why the startup warning below singles them out
// rather than complaining about every difference from the default.
var fanOutCollectors = map[string]bool{"clients": true, "switchports": true, "sensors": true}

// defaultSlowSet is DefaultSlowCollectors parsed once, used to tell an
// explicitly chosen slow list from the built-in default. Compared as a SET and
// never as raw text: a value differing only by whitespace is still the default
// and must not warn, and deployed env files reliably acquire spaces after the
// commas. Panics at init if the constant is malformed, which is a programming
// error rather than a runtime condition.
var defaultSlowSet = func() map[string]bool {
	m, err := parseCollectorSet(DefaultSlowCollectors, "slow collector")
	if err != nil {
		panic("DefaultSlowCollectors is not parseable: " + err.Error())
	}
	return m
}()

// sameSet reports whether two collector sets hold the same names.
func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for name := range a {
		if !b[name] {
			return false
		}
	}
	return true
}

// DefaultSlowCollectors are polled at the slow interval by default: their
// data changes slowly, so frequent refreshes waste Meraki API budget. Only
// takes effect for collectors that are also enabled.
//
// switchports and sensors are here for cost rather than volatility. They fan
// out per port and per device instead of making one org-wide call — a fleet of
// 1,000 switch ports is ~50 paginated requests — so polling them on the fast
// interval would dominate the API budget of anyone who enables them. Same
// reasoning that keeps them out of DefaultCollectors.
var DefaultSlowCollectors = "clients,licenses,apiusage,alerts,switchports,sensors"

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// durationEnv reads a duration from the environment, recording a malformed
// value in errs instead of silently falling back to the default. Silence is
// the wrong behaviour here: the deployment profiles pin specific intervals and
// the alert staleness thresholds are tuned to them, so a typo like "5min"
// would quietly revert to the built-in default and mistune the whole stack.
func durationEnv(errs *[]error, key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("invalid %s %q: want a Go duration such as 30s, 5m or 2h", key, v))
		return def
	}
	return d
}

// validCollectors indexes AllCollectors for lookup, built once rather than
// rebuilt on each parseCollectorSet call.
var validCollectors = func() map[string]bool {
	m := make(map[string]bool, len(AllCollectors))
	for _, c := range AllCollectors {
		m[c] = true
	}
	return m
}()

// parseCollectorSet parses a comma-separated collector list, validating each
// name against AllCollectors. kind appears in error messages ("collector" or
// "slow collector").
func parseCollectorSet(list, kind string) (map[string]bool, error) {
	set := map[string]bool{}
	for _, c := range strings.Split(list, ",") {
		c = strings.TrimSpace(strings.ToLower(c))
		if c == "" {
			continue
		}
		if !validCollectors[c] {
			return nil, fmt.Errorf("unknown %s %q (valid: %s)", kind, c, strings.Join(AllCollectors, ", "))
		}
		set[c] = true
	}
	return set, nil
}

// collectorWarnings reports the two ways COLLECTORS_SLOW quietly does
// something other than what was meant. Neither is an error: both configurations
// are legal, and an existing deployment must not fail to start over them.
func collectorWarnings(enabled, slow map[string]bool) []string {
	var warnings []string

	// 1. A name in the slow list that is not enabled does nothing whatsoever.
	//    parseCollectorSet validates the NAME, so a typo like "client" is
	//    rejected, but "sensors" on a deployment without sensors enabled is
	//    accepted and silently discarded.
	//
	//    Only checked for an explicitly set list. DefaultSlowCollectors names
	//    every opt-in collector on purpose, so that enabling one later puts it
	//    straight into the right group — warning about that would fire on the
	//    shipped defaults, which is how warnings get ignored.
	if !sameSet(slow, defaultSlowSet) {
		var ignored []string
		for name := range slow {
			if !enabled[name] {
				ignored = append(ignored, name)
			}
		}
		if len(ignored) > 0 {
			sort.Strings(ignored)
			warnings = append(warnings, fmt.Sprintf(
				"COLLECTORS_SLOW names %s, which is not in COLLECTORS_ENABLED and has no effect",
				strings.Join(ignored, ", ")))
		}
	}

	// 2. COLLECTORS_SLOW REPLACES the built-in default rather than extending
	//    it, so naming one collector silently pulls the others back into the
	//    fast group. Only flagged for the fan-out collectors, where the cost is
	//    real: moving clients to a 5m interval turns 27 API calls per poll into
	//    a six-fold increase. Deliberately moving a cheap collector such as
	//    alerts to the fast group stays silent.
	var demoted []string
	for name := range defaultSlowSet {
		if enabled[name] && !slow[name] && fanOutCollectors[name] {
			demoted = append(demoted, name)
		}
	}
	if len(demoted) > 0 {
		sort.Strings(demoted)
		warnings = append(warnings, fmt.Sprintf(
			"COLLECTORS_SLOW replaces the built-in default rather than adding to it: %s %s enabled but polling on the FAST interval, "+
				"which multiplies API usage. Restate the whole list if that was not intended",
			strings.Join(demoted, ", "), pluralIs(len(demoted))))
	}
	return warnings
}

func pluralIs(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// Load parses flags and environment variables into a Config.
func Load(args []string) (*Config, error) {
	fs := flag.NewFlagSet("meraki-exporter", flag.ContinueOnError)

	// Collected while building flag defaults and reported after parsing, so
	// every bad value is shown at once rather than one per run.
	var envErrs []error

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
		interval = fs.Duration("scrape.interval", durationEnv(&envErrs, "SCRAPE_INTERVAL", defaultScrapeInterval),
			"Fast-group background poll interval (env: SCRAPE_INTERVAL)")
		slowIntv = fs.Duration("scrape.slow-interval", durationEnv(&envErrs, "SLOW_SCRAPE_INTERVAL", defaultSlowInterval),
			"Slow-group background poll interval (env: SLOW_SCRAPE_INTERVAL)")
		apiTimeout = fs.Duration("meraki.timeout", durationEnv(&envErrs, "MERAKI_TIMEOUT", defaultAPITimeout),
			"Per-request API timeout (env: MERAKI_TIMEOUT)")
		clientsTS = fs.Duration("clients.timespan", durationEnv(&envErrs, "CLIENTS_TIMESPAN", time.Hour),
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
	if len(envErrs) > 0 {
		return nil, errors.Join(envErrs...)
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
		Warnings:        collectorWarnings(enabled, slow),
	}, nil
}
