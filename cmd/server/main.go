// Command server runs the URL shortener HTTP service.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"urlshortener/internal/config"
	"urlshortener/internal/httpsrv"
	"urlshortener/internal/metrics"
	"urlshortener/internal/shortener"
	"urlshortener/internal/store"
)

func main() {
	cfgPath := flag.String("config", os.Getenv("SHORTENER_CONFIG"), "path to config.yaml")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		logger.Error("open db failed", "err", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	m := metrics.New()
	s := httpsrv.New(cfg, db, shortener.New(db), logger, m)

	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	g, ctx := errgroup.WithContext(appCtx)
	g.Go(func() error {
		ln, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}
		logger.Info("listening", "addr", cfg.Listen)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})
	// click worker gets its own ctx: it must keep draining while the HTTP
	// server is still shutting down (in-flight redirects still enqueue)
	workersCtx, workersCancel := context.WithCancel(ctx)
	g.Go(func() error {
		s.Workers(workersCtx)
		return nil
	})
	// daily click retention sweep (config: retention_days)
	g.Go(func() error {
		return s.RetentionLoop(ctx)
	})
	// shutdown order matters: first stop accepting requests, then let the
	// click worker drain its queue, store is closed by main's defer
	g.Go(func() error {
		<-ctx.Done()
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(stopCtx)
		// stop the worker only after Shutdown drained in-flight requests
		workersCancel()
		return err
	})

	if err := g.Wait(); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}

// newLogger builds the root structured logger from the configured level.
func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     lv,
		AddSource: lv == slog.LevelDebug,
	})
	return slog.New(h)
}
