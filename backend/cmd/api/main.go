// Command api serves the wallet REST API.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sovereigntrust/wallet/internal/app"
	"github.com/sovereigntrust/wallet/internal/config"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/httpapi"
	"github.com/sovereigntrust/wallet/internal/ratelimit"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer a.Close()
	if os.Getenv("MIGRATE_ON_START") == "true" {
		if err := db.Migrate(ctx, a.Pool); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		a.Log.Info("migrations applied")
	}

	mem := ratelimit.NewMemory()
	var limiter ratelimit.Limiter = mem
	if a.Redis != nil {
		limiter = &ratelimit.Redis{Client: a.Redis, Fallback: mem, Log: a.Log}
	}
	srv := &httpapi.Server{Cfg: cfg, Pool: a.Pool, Redis: a.Redis, Log: a.Log, Auth: a.Auth, Tokens: a.Tokens,
		Transfers: a.Transfers, Deposits: a.Deposits, Withdrawals: a.Withdrawals, Callbacks: a.Callbacks,
		Statements: a.Statements, Admin: a.Admin, Reconciliation: a.Reconciliation, Providers: a.Providers, Limiter: limiter}

	api := &http.Server{
		Addr: cfg.HTTPAddr, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10,
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.Handler())
	metrics := &http.Server{Addr: cfg.MetricsAddr, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}

	errCh := make(chan error, 2)
	go func() { errCh <- api.ListenAndServe() }()
	go func() { errCh <- metrics.ListenAndServe() }()
	a.Log.Info("api listening", "addr", cfg.HTTPAddr, "metrics", cfg.MetricsAddr, "payments_mode", cfg.PaymentsMode)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	a.Log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = metrics.Shutdown(sctx)
	return api.Shutdown(sctx)
}
