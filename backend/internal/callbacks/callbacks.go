// Package callbacks persists and processes provider callbacks.
//
// Every callback is stored verbatim (deduplicated by a provider-specific key)
// and acknowledged immediately; processing happens in a worker job. Because
// neither Daraja nor (as configured here) Airtel callbacks are
// cryptographically signed, processing never trusts callback content for an
// outcome on its own:
//
//   - Daraja STK and Airtel callbacks trigger an authenticated status query.
//   - Daraja B2C results (no status API exists) are applied only when the
//     ConversationID, OriginatorConversationID and amount all match what
//     Safaricom returned to us for that withdrawal; anything else is flagged.
//
// Out-of-order and duplicate callbacks are harmless: state machines reject
// illegal transitions, ledger postings are keyed, and stale callbacks for
// terminal records are recorded as ignored.
package callbacks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/outbox"
	"github.com/sovereigntrust/wallet/internal/payments/airtel"
	"github.com/sovereigntrust/wallet/internal/payments/mpesa"
	"github.com/sovereigntrust/wallet/internal/withdrawals"
)

const JobProcess = "callback.process"

const (
	KindSTK        = "stk"
	KindB2CResult  = "b2c_result"
	KindB2CTimeout = "b2c_timeout"
	KindAirtel     = "airtel"
)

var callbacksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "wallet_provider_callbacks_total", Help: "Provider callbacks by provider, kind and processing result.",
}, []string{"provider", "kind", "result"})

func init() { prometheus.MustRegister(callbacksTotal) }

// DedupeKey derives the deduplication key from a raw callback body.
func DedupeKey(provider, kind string, raw []byte) (string, error) {
	switch provider + "/" + kind {
	case "mpesa/" + KindSTK:
		cb, err := mpesa.ParseSTKCallback(raw)
		if err != nil {
			return "", err
		}
		return cb.CheckoutRequestID + ":" + cb.ResultCode, nil
	case "mpesa/" + KindB2CResult, "mpesa/" + KindB2CTimeout:
		r, err := mpesa.ParseB2CResult(raw)
		if err != nil {
			return "", err
		}
		return r.ConversationID + ":" + r.OriginatorConversationID + ":" + r.ResultCode, nil
	case "airtel/" + KindAirtel:
		cb, err := airtel.ParseCallback(raw)
		if err != nil {
			return "", err
		}
		return cb.ID + ":" + cb.StatusCode, nil
	}
	return "", fmt.Errorf("unknown callback %s/%s", provider, kind)
}

type Store struct{ Pool *pgxpool.Pool }

// Save persists a callback and enqueues its processing atomically. It
// returns duplicate=true when an identical callback was already stored.
func (s *Store) Save(ctx context.Context, provider, kind string, raw []byte, sourceIP string) (duplicate bool, err error) {
	key, err := DedupeKey(provider, kind, raw)
	if err != nil {
		return false, apperr.Validation("malformed callback: "+err.Error(), nil)
	}
	if !json.Valid(raw) {
		return false, apperr.Validation("malformed callback JSON", nil)
	}
	id := uuid.Must(uuid.NewV7())
	err = db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO provider_callbacks (id, provider, kind, dedupe_key, payload, source_ip)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (provider, kind, dedupe_key) DO NOTHING`,
			id, provider, kind, key, raw, sourceIP)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			duplicate = true
			return nil
		}
		return outbox.Enqueue(ctx, tx, JobProcess, map[string]string{"callback_id": id.String()},
			outbox.EnqueueOpts{DedupeKey: JobProcess + ":" + id.String(), MaxAttempts: 20})
	})
	if duplicate {
		callbacksTotal.WithLabelValues(provider, kind, "duplicate").Inc()
	}
	return duplicate, err
}

type Processor struct {
	Pool        *pgxpool.Pool
	Deposits    *deposits.Service
	Withdrawals *withdrawals.Service
	Log         *slog.Logger
}

func (p *Processor) finish(ctx context.Context, id uuid.UUID, provider, kind, status, result string) error {
	callbacksTotal.WithLabelValues(provider, kind, status).Inc()
	_, err := p.Pool.Exec(ctx, `UPDATE provider_callbacks SET status = $2, result = left($3, 1000), processed_at = now() WHERE id = $1`, id, status, result)
	return err
}

// Process handles one stored callback (outbox job).
func (p *Processor) Process(ctx context.Context, id uuid.UUID) error {
	var provider, kind, status string
	var raw []byte
	err := p.Pool.QueryRow(ctx, `SELECT provider, kind, status, payload FROM provider_callbacks WHERE id = $1`, id).Scan(&provider, &kind, &status, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return outbox.Permanent(err)
	}
	if err != nil {
		return err
	}
	if status != "received" {
		return nil
	}
	var result, outcome string
	switch provider + "/" + kind {
	case "mpesa/" + KindSTK:
		outcome, result, err = p.processSTK(ctx, raw)
	case "mpesa/" + KindB2CResult:
		outcome, result, err = p.processB2C(ctx, raw)
	case "mpesa/" + KindB2CTimeout:
		outcome, result, err = p.processB2CTimeout(ctx, raw)
	case "airtel/" + KindAirtel:
		outcome, result, err = p.processAirtel(ctx, raw)
	default:
		outcome, result = "ignored", "unknown callback kind"
	}
	if err != nil {
		p.Log.Error("callback processing failed", "callback_id", id, "err", err)
		return err
	}
	return p.finish(ctx, id, provider, kind, outcome, result)
}

func (p *Processor) processSTK(ctx context.Context, raw []byte) (string, string, error) {
	cb, err := mpesa.ParseSTKCallback(raw)
	if err != nil {
		return "ignored", "unparseable: " + err.Error(), nil
	}
	d, err := deposits.FindByCheckoutRef(ctx, p.Pool, "mpesa", cb.CheckoutRequestID)
	if apperr.Is(err, apperr.CodeNotFound) {
		return "ignored", "no deposit for CheckoutRequestID (reconciliation will review)", nil
	}
	if err != nil {
		return "", "", err
	}
	if cb.ResultCode == "0" && cb.Amount != 0 && cb.Amount != d.Amount {
		if err := deposits.FlagReview(ctx, p.Pool, d.ID); err != nil {
			return "", "", err
		}
		p.Log.Warn("STK callback amount mismatch", "deposit_id", d.ID, "callback_amount", cb.Amount, "expected", d.Amount)
	}
	res, err := p.Deposits.Confirm(ctx, d.ID, cb.Receipt)
	return "processed", res, err
}

func (p *Processor) processB2C(ctx context.Context, raw []byte) (string, string, error) {
	r, err := mpesa.ParseB2CResult(raw)
	if err != nil {
		return "ignored", "unparseable: " + err.Error(), nil
	}
	w, err := withdrawals.FindByConversationRef(ctx, p.Pool, "mpesa", r.ConversationID)
	if apperr.Is(err, apperr.CodeNotFound) {
		// The result may race ahead of our own commit of the ConversationID.
		w, err = withdrawals.FindByRequestRef(ctx, p.Pool, "mpesa", r.OriginatorConversationID)
		if apperr.Is(err, apperr.CodeNotFound) {
			return "ignored", "no withdrawal matches conversation identifiers", nil
		}
		if err != nil {
			return "", "", err
		}
		if w.ProviderConvRef == nil && w.Status == "processing" {
			return "", "", outbox.RetryAfter(5*time.Second, "withdrawal conversation id not yet recorded")
		}
	}
	if err != nil {
		return "", "", err
	}
	matched := w.ProviderConvRef != nil && *w.ProviderConvRef == r.ConversationID &&
		w.ProviderRequestRef != nil && *w.ProviderRequestRef == r.OriginatorConversationID &&
		(r.Amount == 0 || r.Amount == w.Amount)
	if !matched {
		if err := withdrawals.FlagReview(ctx, p.Pool, w.ID); err != nil {
			return "", "", err
		}
		return "ignored", "identifiers or amount do not match; withdrawal flagged for review", nil
	}
	res := mpesa.NormalizeB2CResult(r)
	if err := p.Withdrawals.ApplyOutcome(ctx, w.ID, res); err != nil {
		return "", "", err
	}
	return "processed", "applied " + string(res.Outcome), nil
}

func (p *Processor) processB2CTimeout(ctx context.Context, raw []byte) (string, string, error) {
	r, err := mpesa.ParseB2CResult(raw)
	if err != nil {
		return "ignored", "unparseable: " + err.Error(), nil
	}
	w, err := withdrawals.FindByRequestRef(ctx, p.Pool, "mpesa", r.OriginatorConversationID)
	if apperr.Is(err, apperr.CodeNotFound) {
		return "ignored", "no matching withdrawal", nil
	}
	if err != nil {
		return "", "", err
	}
	return "processed", "marked uncertain", p.Withdrawals.MarkTimeout(ctx, w.ID, r.ResultDesc)
}

func (p *Processor) processAirtel(ctx context.Context, raw []byte) (string, string, error) {
	cb, err := airtel.ParseCallback(raw)
	if err != nil {
		return "ignored", "unparseable: " + err.Error(), nil
	}
	if d, err := deposits.FindByRequestRef(ctx, p.Pool, "airtel", cb.ID); err == nil {
		res, err := p.Deposits.Confirm(ctx, d.ID, cb.AirtelMoneyID)
		return "processed", res, err
	} else if !apperr.Is(err, apperr.CodeNotFound) {
		return "", "", err
	}
	if w, err := withdrawals.FindByRequestRef(ctx, p.Pool, "airtel", cb.ID); err == nil {
		return "processed", "status recheck scheduled", withdrawals.RequestRecheck(ctx, p.Pool, w.ID)
	} else if !apperr.Is(err, apperr.CodeNotFound) {
		return "", "", err
	}
	return "ignored", "no matching transaction", nil
}
