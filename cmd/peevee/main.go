package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/OdedNeuhaus/peevee/internal/api"
	"github.com/OdedNeuhaus/peevee/internal/app"
	"github.com/OdedNeuhaus/peevee/internal/cluster"
	"github.com/OdedNeuhaus/peevee/internal/collector"
	"github.com/OdedNeuhaus/peevee/internal/config"
	"github.com/OdedNeuhaus/peevee/internal/remotewrite"
	"github.com/OdedNeuhaus/peevee/internal/store"
	"github.com/OdedNeuhaus/peevee/internal/version"
	"github.com/OdedNeuhaus/peevee/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "peevee: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", envOr("PEEVEE_CONFIG", "/etc/peevee/config.yaml"), "path to the configuration file")
		logLevel    = flag.String("log-level", envOr("PEEVEE_LOG_LEVEL", "info"), "log level: debug, info, warn or error")
		logFormat   = flag.String("log-format", envOr("PEEVEE_LOG_FORMAT", "text"), "log format: text or json")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("peevee %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		return nil
	}

	log := newLogger(*logLevel, *logFormat)
	log.Info("starting peevee", "version", version.Version, "commit", version.Commit, "config", *configPath)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	log.Info("configuration loaded",
		"source", cfg.SourceName(),
		"interval", cfg.Collector.Interval,
		"kubeconfigDir", cfg.Discovery.Dir,
		"remoteWrite", cfg.RemoteWrite.Enabled)

	pool := cluster.NewPool(cfg, log)
	if err := pool.Reload(); err != nil {
		return fmt.Errorf("discover clusters: %w", err)
	}
	found := pool.Clusters()
	if len(found) == 0 {
		// Not fatal: an operator may be about to populate the kubeconfig secret,
		// and the reload watcher will pick it up without a restart.
		log.Warn("no clusters discovered; check the kubeconfig directory",
			"dir", cfg.Discovery.Dir, "loadErrors", pool.LoadErrors())
	} else {
		for _, c := range found {
			log.Info("cluster discovered", "name", c.Name, "endpoint", c.Endpoint, "source", c.Source)
		}
	}

	rw, err := remotewrite.New(cfg.RemoteWrite, log)
	if err != nil {
		return fmt.Errorf("configure remote write: %w", err)
	}

	st := store.New()
	col := collector.New(pool, cfg, log)
	runner := app.NewRunner(col, st, pool, rw, cfg, log)

	webFS, err := fs.Sub(web.Assets, "ui")
	if err != nil {
		return fmt.Errorf("mount embedded ui: %w", err)
	}

	server := api.NewServer(st, pool, cfg, rw, runner, webFS, log)
	addr := net.JoinHostPort(cfg.Server.ListenAddress, strconv.Itoa(cfg.Server.Port))
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the server-sent events stream is intentionally
		// long-lived and any deadline here would sever it.
		IdleTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go runner.Run(ctx)
	go pool.Watch(ctx)

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	log.Info("stopped")
	return nil
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
