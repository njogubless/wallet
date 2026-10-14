// Package policy evaluates configurable fees and transaction limits.
// Rules are versioned rows; the one in force is the latest whose
// effective_from has passed.
package policy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/money"
)

type FeeRule struct {
	ID            uuid.UUID  `json:"id"`
	Operation     string     `json:"operation"`
	Provider      string     `json:"provider"`
	Currency      string     `json:"currency"`
	FlatFee       int64      `json:"flat_fee_minor"`
	RateBps       int        `json:"rate_bps"`
	MinFee        int64      `json:"min_fee_minor"`
	MaxFee        *int64     `json:"max_fee_minor"`
	EffectiveFrom time.Time  `json:"effective_from"`
	Reason        string     `json:"reason"`
	CreatedBy     *uuid.UUID `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Compute returns flat + ceil(amount*rate), clamped to [min, max].
func (r FeeRule) Compute(amount int64) (int64, error) {
	pct, err := money.ApplyBasisPoints(amount, r.RateBps)
	if err != nil {
		return 0, err
	}
	fee, err := money.Add(r.FlatFee, pct)
	if err != nil {
		return 0, err
	}
	if fee < r.MinFee {
		fee = r.MinFee
	}
	if r.MaxFee != nil && fee > *r.MaxFee {
		fee = *r.MaxFee
	}
	return fee, nil
}

const feeCols = `id, operation, provider, currency, flat_fee, rate_bps, min_fee, max_fee, effective_from, reason, created_by, created_at`

func scanFee(row pgx.Row) (FeeRule, error) {
	var r FeeRule
	err := row.Scan(&r.ID, &r.Operation, &r.Provider, &r.Currency, &r.FlatFee, &r.RateBps, &r.MinFee, &r.MaxFee, &r.EffectiveFrom, &r.Reason, &r.CreatedBy, &r.CreatedAt)
	return r, err
}

// CurrentFeeRule picks the provider-specific rule, falling back to '*'.
func CurrentFeeRule(ctx context.Context, q db.Querier, op, provider, currency string) (FeeRule, error) {
	r, err := scanFee(q.QueryRow(ctx, `
		SELECT `+feeCols+` FROM fee_rules
		 WHERE operation = $1 AND currency = $3 AND provider IN ($2, '*') AND effective_from <= now()
		 ORDER BY (provider = $2) DESC, effective_from DESC, created_at DESC LIMIT 1`, op, provider, currency))
	if errors.Is(err, pgx.ErrNoRows) {
		return FeeRule{}, fmt.Errorf("no fee rule configured for %s/%s/%s", op, provider, currency)
	}
	return r, err
}

// Fee computes the fee for an operation.
func Fee(ctx context.Context, q db.Querier, op, provider, currency string, amount int64) (int64, error) {
	r, err := CurrentFeeRule(ctx, q, op, provider, currency)
	if err != nil {
		return 0, err
	}
	return r.Compute(amount)
}

type LimitRule struct {
	ID            uuid.UUID  `json:"id"`
	KYCTier       int        `json:"kyc_tier"`
	Operation     string     `json:"operation"`
	Currency      string     `json:"currency"`
	PerTxnMin     int64      `json:"per_txn_min_minor"`
	PerTxnMax     int64      `json:"per_txn_max_minor"`
	DailyMax      int64      `json:"daily_max_minor"`
	EffectiveFrom time.Time  `json:"effective_from"`
	Reason        string     `json:"reason"`
	CreatedBy     *uuid.UUID `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
}

const limitCols = `id, kyc_tier, operation, currency, per_txn_min, per_txn_max, daily_max, effective_from, reason, created_by, created_at`

func scanLimit(row pgx.Row) (LimitRule, error) {
	var r LimitRule
	err := row.Scan(&r.ID, &r.KYCTier, &r.Operation, &r.Currency, &r.PerTxnMin, &r.PerTxnMax, &r.DailyMax, &r.EffectiveFrom, &r.Reason, &r.CreatedBy, &r.CreatedAt)
	return r, err
}

func CurrentLimitRule(ctx context.Context, q db.Querier, tier int, op, currency string) (LimitRule, error) {
	r, err := scanLimit(q.QueryRow(ctx, `
		SELECT `+limitCols+` FROM limit_rules
		 WHERE kyc_tier = $1 AND operation = $2 AND currency = $3 AND effective_from <= now()
		 ORDER BY effective_from DESC, created_at DESC LIMIT 1`, tier, op, currency))
	if errors.Is(err, pgx.ErrNoRows) {
		return LimitRule{}, apperr.New(apperr.CodeLimitExceeded, "this operation is not available for your verification tier")
	}
	return r, err
}

// DailyUsed sums today's (Africa/Nairobi calendar day) non-failed amounts for
// an operation. Call it after locking the user's wallet row so concurrent
// requests cannot both pass the check.
func DailyUsed(ctx context.Context, q db.Querier, userID uuid.UUID, op, currency string) (int64, error) {
	var query string
	switch op {
	case "transfer":
		query = `SELECT coalesce(sum(amount), 0) FROM transfers
		          WHERE sender_user_id = $1 AND currency = $2 AND status IN ('created','processing','succeeded')
		            AND created_at >= ` + startOfDayNairobi
	case "withdrawal":
		query = `SELECT coalesce(sum(amount), 0) FROM withdrawals
		          WHERE user_id = $1 AND currency = $2 AND status IN ('created','pending','processing','succeeded')
		            AND created_at >= ` + startOfDayNairobi
	case "deposit":
		query = `SELECT coalesce(sum(amount), 0) FROM deposits
		          WHERE user_id = $1 AND currency = $2 AND status IN ('created','pending_provider','succeeded')
		            AND created_at >= ` + startOfDayNairobi
	default:
		return 0, fmt.Errorf("unknown operation %q", op)
	}
	var used int64
	err := q.QueryRow(ctx, query, userID, currency).Scan(&used)
	return used, err
}

const startOfDayNairobi = `(date_trunc('day', now() AT TIME ZONE 'Africa/Nairobi') AT TIME ZONE 'Africa/Nairobi')`

// CheckLimits enforces per-transaction and daily limits.
func CheckLimits(ctx context.Context, q db.Querier, userID uuid.UUID, tier int, op, currency string, amount int64) error {
	r, err := CurrentLimitRule(ctx, q, tier, op, currency)
	if err != nil {
		return err
	}
	cur, _ := money.LookupCurrency(currency)
	if amount < r.PerTxnMin {
		return &apperr.Error{Code: apperr.CodeLimitExceeded,
			Message: fmt.Sprintf("minimum amount is %s %s", currency, money.Format(r.PerTxnMin, cur))}
	}
	if amount > r.PerTxnMax {
		return &apperr.Error{Code: apperr.CodeLimitExceeded,
			Message: fmt.Sprintf("maximum per transaction is %s %s", currency, money.Format(r.PerTxnMax, cur))}
	}
	used, err := DailyUsed(ctx, q, userID, op, currency)
	if err != nil {
		return err
	}
	if used+amount > r.DailyMax {
		remaining := max(r.DailyMax-used, 0)
		return &apperr.Error{Code: apperr.CodeLimitExceeded,
			Message: fmt.Sprintf("daily limit exceeded; remaining today: %s %s", currency, money.Format(remaining, cur))}
	}
	return nil
}

// Summary describes a user's limits and usage for display.
type Summary struct {
	Operation string      `json:"operation"`
	Currency  string      `json:"currency"`
	PerTxnMin string      `json:"per_transaction_min"`
	PerTxnMax string      `json:"per_transaction_max"`
	DailyMax  string      `json:"daily_max"`
	UsedToday string      `json:"used_today"`
	Remaining string      `json:"remaining_today"`
	Fee       *FeeSummary `json:"fee,omitempty"`
}

type FeeSummary struct {
	FlatFee string  `json:"flat_fee"`
	RateBps int     `json:"rate_bps"`
	MinFee  string  `json:"min_fee"`
	MaxFee  *string `json:"max_fee"`
}

func Summaries(ctx context.Context, q db.Querier, userID uuid.UUID, tier int, currency string) ([]Summary, error) {
	cur, err := money.LookupCurrency(currency)
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, op := range []string{"deposit", "transfer", "withdrawal"} {
		r, err := CurrentLimitRule(ctx, q, tier, op, currency)
		if err != nil {
			if apperr.Is(err, apperr.CodeLimitExceeded) {
				continue
			}
			return nil, err
		}
		used, err := DailyUsed(ctx, q, userID, op, currency)
		if err != nil {
			return nil, err
		}
		s := Summary{
			Operation: op, Currency: currency,
			PerTxnMin: money.Format(r.PerTxnMin, cur), PerTxnMax: money.Format(r.PerTxnMax, cur),
			DailyMax: money.Format(r.DailyMax, cur), UsedToday: money.Format(used, cur),
			Remaining: money.Format(max(r.DailyMax-used, 0), cur),
		}
		if fr, err := CurrentFeeRule(ctx, q, op, "*", currency); err == nil {
			fs := &FeeSummary{FlatFee: money.Format(fr.FlatFee, cur), RateBps: fr.RateBps, MinFee: money.Format(fr.MinFee, cur)}
			if fr.MaxFee != nil {
				m := money.Format(*fr.MaxFee, cur)
				fs.MaxFee = &m
			}
			s.Fee = fs
		}
		out = append(out, s)
	}
	return out, nil
}

// History lists rule versions for admin views.
func FeeHistory(ctx context.Context, q db.Querier) ([]FeeRule, error) {
	rows, err := q.Query(ctx, `SELECT `+feeCols+` FROM fee_rules ORDER BY operation, provider, effective_from DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (FeeRule, error) { return scanFee(r) })
}

func LimitHistory(ctx context.Context, q db.Querier) ([]LimitRule, error) {
	rows, err := q.Query(ctx, `SELECT `+limitCols+` FROM limit_rules ORDER BY kyc_tier, operation, effective_from DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LimitRule, error) { return scanLimit(r) })
}

func InsertFeeRule(ctx context.Context, q db.Querier, r FeeRule) (FeeRule, error) {
	if r.EffectiveFrom.IsZero() {
		r.EffectiveFrom = time.Now()
	}
	return scanFee(q.QueryRow(ctx, `
		INSERT INTO fee_rules (id, operation, provider, currency, flat_fee, rate_bps, min_fee, max_fee, effective_from, reason, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+feeCols,
		uuid.Must(uuid.NewV7()), r.Operation, r.Provider, r.Currency, r.FlatFee, r.RateBps, r.MinFee, r.MaxFee, r.EffectiveFrom, r.Reason, r.CreatedBy))
}

func InsertLimitRule(ctx context.Context, q db.Querier, r LimitRule) (LimitRule, error) {
	if r.EffectiveFrom.IsZero() {
		r.EffectiveFrom = time.Now()
	}
	return scanLimit(q.QueryRow(ctx, `
		INSERT INTO limit_rules (id, kyc_tier, operation, currency, per_txn_min, per_txn_max, daily_max, effective_from, reason, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+limitCols,
		uuid.Must(uuid.NewV7()), r.KYCTier, r.Operation, r.Currency, r.PerTxnMin, r.PerTxnMax, r.DailyMax, r.EffectiveFrom, r.Reason, r.CreatedBy))
}
