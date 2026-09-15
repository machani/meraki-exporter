// meraki-exporter exposes Cisco Meraki Dashboard API metrics in
// Prometheus format.
//
// Architecture: a background poller refreshes data from the Meraki API
// every --scrape.interval and caches const-metrics; GET /metrics serves
// the cache instantly, keeping Prometheus scrapes fast and decoupled
// from Meraki's rate limits.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promcollectors "github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"

	"github.com/machani/meraki-exporter/internal/collector"
	"github.com/machani/meraki-exporter/internal/config"
	"github.com/machani/meraki-exporter/internal/meraki"
)

// version is overridden at build time via
// -ldflags "-X main.version=<version>". The default
// deliberately reads as a non-release so unstamped local builds are obvious.
var version = "dev"

// shutdownTimeout bounds the graceful HTTP shutdown after SIGTERM/interrupt.
const shutdownTimeout = 5 * time.Second

func main() {
	os.Exit(run())
}

// run holds main's logic and returns the process exit code, so deferred
// cleanups execute before os.Exit.
func run() int {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		slog.Error("configuration error", "err", err)
		return 1
	}

	level := slog.LevelInfo
	if cfg.LogLevel == "debug" {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// Split enabled collectors into fast and slow poll groups.
	var fastNames, slowNames []string
	for _, name := range config.AllCollectors {
		if !cfg.Collectors[name] {
			continue
		}
		if cfg.SlowCollectors[name] {
			slowNames = append(slowNames, name)
		} else {
			fastNames = append(fastNames, name)
		}
	}

	logger.Info("starting meraki-exporter",
		"version", version,
		"listen", cfg.ListenAddress,
		"fast_interval", cfg.ScrapeInterval,
		"slow_interval", cfg.SlowInterval,
		"fast_collectors", fastNames,
		"slow_collectors", slowNames,
		"proxy_configured", cfg.ProxyURL != "")

	// Logged after the startup line so the resolved collector groups above give
	// them context. Warnings rather than errors: both configurations they
	// describe are legal, and an existing deployment must not fail to start.
	for _, w := range cfg.Warnings {
		logger.Warn(w)
	}

	// baseReg holds process/runtime metrics and the exporter's own API
	// request counter; it is served on /metrics and /metrics/fast.
	baseReg := prometheus.NewRegistry()
	baseReg.MustRegister(
		promcollectors.NewGoCollector(),
		promcollectors.NewProcessCollector(promcollectors.ProcessCollectorOpts{}),
		promcollectors.NewBuildInfoCollector(),
	)
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "meraki",
		Subsystem: "exporter",
		Name:      "build_info",
		Help:      "Exporter build information; value is always 1.",
	}, []string{"version", "goversion"})
	buildInfo.WithLabelValues(version, runtime.Version()).Set(1)
	baseReg.MustRegister(buildInfo)

	client, err := meraki.New(cfg.BaseURL, cfg.APIKey, cfg.APITimeout, "meraki-exporter/"+version, cfg.ProxyURL, logger, baseReg)
	if err != nil {
		logger.Error("failed to build Meraki client", "err", err)
		return 1
	}

	fastExp := collector.New(cfg, client, logger, "fast", fastNames, cfg.ScrapeInterval)
	slowExp := collector.New(cfg, client, logger, "slow", slowNames, cfg.SlowInterval)

	fastReg := prometheus.NewRegistry()
	fastReg.MustRegister(fastExp)
	slowReg := prometheus.NewRegistry()
	slowReg.MustRegister(slowExp)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go fastExp.Run(ctx)
	go slowExp.Run(ctx)

	opts := promhttp.HandlerOpts{
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	mux := http.NewServeMux()
	// Combined view (backwards compatible) plus split endpoints so
	// Prometheus can scrape fast and slow groups at different intervals.
	// Scrape EITHER /metrics OR the /metrics/fast + /metrics/slow pair —
	// scraping both into one Prometheus duplicates series.
	mux.Handle("/metrics", promhttp.HandlerFor(prometheus.Gatherers{baseReg, fastReg, slowReg}, opts))
	mux.Handle("/metrics/fast", promhttp.HandlerFor(prometheus.Gatherers{baseReg, fastReg}, opts))
	mux.Handle("/metrics/slow", promhttp.HandlerFor(slowReg, opts))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !fastExp.Ready() || !slowExp.Ready() {
			http.Error(w, "first poll not yet complete", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Meraki Exporter</title></head><body>
<h1>meraki-exporter</h1>
<p><a href="/metrics">Metrics (all)</a> &middot; <a href="/metrics/fast">Fast group</a> &middot;
<a href="/metrics/slow">Slow group</a> &middot; <a href="/healthz">Health</a></p>
</body></html>`))
	})

	// Timeouts bound a slow or stalled client. WriteTimeout is generous
	// because /metrics is served from cache and is small (hundreds of KB even
	// for a large org), so it only ever trips on a genuinely stuck connection,
	// never on a legitimate scrape — Prometheus defaults to a 10s scrape
	// timeout of its own.
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// exporter-toolkit serves plain HTTP when --web.config.file is empty,
	// and TLS and/or basic auth when a web config file is provided. Note:
	// basic auth also protects /healthz, so probes need credentials too.
	webFlags := &web.FlagConfig{
		WebListenAddresses: &[]string{cfg.ListenAddress},
		WebSystemdSocket:   new(bool),
		WebConfigFile:      &cfg.WebConfigFile,
	}
	if err := web.ListenAndServe(srv, webFlags, logger); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP server error", "err", err)
		return 1
	}
	return 0
}
