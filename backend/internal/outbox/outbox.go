// Package outbox is a PostgreSQL-backed transactional outbox and job runner.
//
// Jobs are enqueued in the same transaction as the state change that requires
// them, so a committed state change always has its follow-up work recorded.
// Workers claim jobs with FOR UPDATE SKIP LOCKED and a lease; a crashed
// worker's jobs are reclaimed when the lease expires. Handlers must therefore
// be idempotent.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sovereigntrust/wallet/internal/db"
)

type EnqueueOpts struct {
	DedupeKey   string
	RunAt       time.Time
	MaxAttempts int
}

// Enqueue records a job. With a DedupeKey, a second enqueue of the same key
// is a no-op.
func Enqueue(ctx context.Context, q db.Querier, kind string, payload any, opts EnqueueOpts) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if opts.RunAt.IsZero() {
		opts.RunAt = time.Now()
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 10
	}
	var dedupe *string
	if opts.DedupeKey != "" {
		dedupe = &opts.DedupeKey
	}
	_, err = q.Exec(ctx, `
		INSERT INTO outbox_jobs (kind, payload, dedupe_key, run_at, max_attempts)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (dedupe_key) DO NOTHING`, kind, body, dedupe, opts.RunAt, opts.MaxAttempts)
	return err
}

type Job struct {
	ID          int64
	Kind        string
	Payload     json.RawMessage
	Attempts    int
	MaxAttempts int
}

// Handler processes one job. Return nil when done, a *RetryError to retry
// later, or a *PermanentError to stop retrying. Other errors are treated as
// transient and retried with exponential backoff.
type Handler func(ctx context.Context, job Job) error

type RetryError struct {
	After  time.Duration
	Reason string
	// Count controls whether this retry consumes an attempt. Status polling
	// that is merely "still pending" should not exhaust the attempt budget.
	Count bool
}

func (e *RetryError) Error() string { return "retry after " + e.After.String() + ": " + e.Reason }

// RetryAfter schedules the job again without consuming an attempt.
func RetryAfter(d time.Duration, reason string) error {
	return &RetryError{After: d, Reason: reason}
}

type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "permanent: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

func Permanent(err error) error { return &PermanentError{Err: err} }

var (
	jobsProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wallet_outbox_jobs_total", Help: "Outbox jobs processed by kind and result.",
	}, []string{"kind", "result"})
	jobDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "wallet_outbox_job_duration_seconds", Help: "Outbox job handler duration.",
		Buckets: prometheus.DefBuckets,
	}, []string{"kind"})
)

func init() { prometheus.MustRegister(jobsProcessed, jobDuration) }

type Runner struct {
	Pool         *pgxpool.Pool
	Log          *slog.Logger
	WorkerID     string
	Handlers     map[string]Handler
	BatchSize    int
	Concurrency  int
	PollInterval time.Duration
	Lease        time.Duration
}

// Run processes jobs until ctx is cancelled, then waits for in-flight jobs.
func (r *Runner) Run(ctx context.Context) error {
	if r.BatchSize <= 0 {
		r.BatchSize = 20
	}
	if r.Concurrency <= 0 {
		r.Concurrency = 8
	}
	if r.PollInterval <= 0 {
		r.PollInterval = time.Second
	}
	if r.Lease <= 0 {
		r.Lease = 2 * time.Minute
	}
	sem := make(chan struct{}, r.Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		if ctx.Err() != nil {
			return nil
		}
		jobs, err := r.claim(ctx)
		if err != nil && ctx.Err() == nil {
			r.Log.Error("claim jobs", "err", err)
		}
		for _, j := range jobs {
			sem <- struct{}{}
			wg.Add(1)
			go func(j Job) {
				defer wg.Done()
				defer func() { <-sem }()
				// Let in-flight jobs finish even during shutdown.
				r.process(context.WithoutCancel(ctx), j)
			}(j)
		}
		if len(jobs) < r.BatchSize {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(r.PollInterval):
			}
		}
	}
}

func (r *Runner) claim(ctx context.Context) ([]Job, error) {
	kinds := make([]string, 0, len(r.Handlers))
	for k := range r.Handlers {
		kinds = append(kinds, k)
	}
	rows, err := r.Pool.Query(ctx, `
		UPDATE outbox_jobs SET status = 'running', locked_until = now() + $1::interval, locked_by = $2,
		       attempts = attempts + 1, updated_at = now()
		 WHERE id IN (
		       SELECT id FROM outbox_jobs
		        WHERE kind = ANY($3)
		          AND ((status = 'pending' AND run_at <= now()) OR (status = 'running' AND locked_until < now()))
		        ORDER BY run_at
		        LIMIT $4
		        FOR UPDATE SKIP LOCKED)
		RETURNING id, kind, payload, attempts, max_attempts`,
		fmt.Sprintf("%d seconds", int(r.Lease.Seconds())), r.WorkerID, kinds, r.BatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.Payload, &j.Attempts, &j.MaxAttempts); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (r *Runner) process(ctx context.Context, j Job) {
	h := r.Handlers[j.Kind]
	start := time.Now()
	hctx, cancel := context.WithTimeout(ctx, r.Lease-5*time.Second)
	err := safeCall(hctx, h, j)
	cancel()
	jobDuration.WithLabelValues(j.Kind).Observe(time.Since(start).Seconds())
	log := r.Log.With("job_id", j.ID, "kind", j.Kind, "attempt", j.Attempts)

	var retry *RetryError
	var perm *PermanentError
	switch {
	case err == nil:
		r.finish(ctx, j.ID, "done", "", time.Time{}, false)
		jobsProcessed.WithLabelValues(j.Kind, "done").Inc()
	case errors.As(err, &retry):
		r.finish(ctx, j.ID, "pending", retry.Reason, time.Now().Add(retry.After), !retry.Count)
		jobsProcessed.WithLabelValues(j.Kind, "rescheduled").Inc()
	case errors.As(err, &perm) || j.Attempts >= j.MaxAttempts:
		log.Error("job failed permanently", "err", err)
		r.finish(ctx, j.ID, "dead", err.Error(), time.Time{}, false)
		jobsProcessed.WithLabelValues(j.Kind, "dead").Inc()
	default:
		backoff := Backoff(j.Attempts)
		log.Warn("job failed; will retry", "err", err, "backoff", backoff)
		r.finish(ctx, j.ID, "pending", err.Error(), time.Now().Add(backoff), false)
		jobsProcessed.WithLabelValues(j.Kind, "retry").Inc()
	}
}

func safeCall(ctx context.Context, h Handler, j Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return h(ctx, j)
}

func (r *Runner) finish(ctx context.Context, id int64, status, lastErr string, runAt time.Time, refundAttempt bool) {
	var runAtArg *time.Time
	if !runAt.IsZero() {
		runAtArg = &runAt
	}
	if len(lastErr) > 1000 {
		lastErr = lastErr[:1000]
	}
	_, err := r.Pool.Exec(ctx, `
		UPDATE outbox_jobs
		   SET status = $2, last_error = NULLIF($3, ''), run_at = coalesce($4, run_at),
		       attempts = attempts - CASE WHEN $5 THEN 1 ELSE 0 END,
		       locked_until = NULL, locked_by = NULL, updated_at = now()
		 WHERE id = $1 AND locked_by = $6`, id, status, lastErr, runAtArg, refundAttempt, r.WorkerID)
	if err != nil {
		r.Log.Error("update job status", "job_id", id, "err", err)
	}
}

// Backoff returns exponential backoff with jitter, capped at 10 minutes.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Second << min(attempt, 10)
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}
