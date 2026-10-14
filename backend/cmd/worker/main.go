// Command worker runs outbox jobs (provider dispatch, status polling,
// callback processing) and periodic maintenance (sweeps, reconciliation,
// metrics, idempotency cleanup).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sovereigntrust/wallet/internal/app"
	"github.com/sovereigntrust/wallet/internal/callbacks"
	"github.com/sovereigntrust/wallet/internal/config"
	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/outbox"
	"github.com/sovereigntrust/wallet/internal/withdrawals"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func idFrom(job outbox.Job, field string) (uuid.UUID, error) {
	var p map[string]string
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return uuid.Nil, outbox.Permanent(err)
	}
	id, err := uuid.Parse(p[field])
	if err != nil {
		return uuid.Nil, outbox.Permanent(fmt.Errorf("bad %s: %w", field, err))
	}
	return id, nil
}

func handler(field string, fn func(context.Context, uuid.UUID) error) outbox.Handler {
	return func(ctx context.Context, job outbox.Job) error {
		id, err := idFrom(job, field)
		if err != nil {
			return err
		}
		return fn(ctx, id)
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

	host, _ := os.Hostname()
	runner := &outbox.Runner{
		Pool: a.Pool, Log: a.Log, WorkerID: fmt.Sprintf("%s-%d", host, os.Getpid()),
		Handlers: map[string]outbox.Handler{
			deposits.JobDispatch:    handler("deposit_id", a.Deposits.Dispatch),
			deposits.JobPoll:        handler("deposit_id", a.Deposits.Poll),
			withdrawals.JobDispatch: handler("withdrawal_id", a.Withdrawals.Dispatch),
			withdrawals.JobPoll:     handler("withdrawal_id", a.Withdrawals.Poll),
			callbacks.JobProcess:    handler("callback_id", a.Processor.Process),
		},
	}

	metricsAddr := os.Getenv("WORKER_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":9091"
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	msrv := &http.Server{Addr: metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = msrv.ListenAndServe() }()

	go periodic(ctx, a, 30*time.Second, "sweep", func(ctx context.Context) error {
		if err := a.Deposits.Sweep(ctx); err != nil {
			return err
		}
		return a.Withdrawals.Sweep(ctx)
	})
	go periodic(ctx, a, 30*time.Second, "gauges", a.Reconciliation.CollectGauges)
	go periodic(ctx, a, 15*time.Minute, "reconcile", func(ctx context.Context) error {
		_, err := a.Reconciliation.RunInternal(ctx, nil)
		return err
	})
	go periodic(ctx, a, time.Hour, "idempotency_purge", func(ctx context.Context) error {
		_, err := a.Idem.Purge(ctx)
		return err
	})

	a.Log.Info("worker started", "worker_id", runner.WorkerID, "payments_mode", cfg.PaymentsMode)
	err = runner.Run(ctx)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = msrv.Shutdown(sctx)
	a.Log.Info("worker stopped")
	return err
}

// periodic runs fn on an interval. Each tick is guarded by a PostgreSQL
// advisory lock so only one worker replica performs it.
func periodic(ctx context.Context, a *app.App, every time.Duration, name string, fn func(context.Context) error) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		func() {
			conn, err := a.Pool.Acquire(ctx)
			if err != nil {
				return
			}
			defer conn.Release()
			var got bool
			if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, "periodic:"+name).Scan(&got); err != nil || !got {
				return
			}
			defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, "periodic:"+name)
			tctx, cancel := context.WithTimeout(ctx, every)
			defer cancel()
			if err := fn(tctx); err != nil && ctx.Err() == nil {
				a.Log.Error("periodic task failed", "task", name, "err", err)
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
