// Package deposits implements mobile-money collections into wallets.
//
// Lifecycle (status / provider_status):
//
//	created/not_sent ──dispatch──▶ created/sending ──accepted──▶ pending_provider/accepted
//	                                     │ rejected ─────────────▶ failed/rejected
//	                                     │ uncertain/crash ──────▶ pending_provider/unknown
//	pending_provider ──confirmed success──▶ succeeded (ledger posted once)
//	pending_provider ──confirmed failure──▶ failed
//	pending_provider ──no definitive answer by expiry──▶ expired (+needs_review)
//	expired ──late confirmed success──▶ succeeded;  succeeded ──approved reversal──▶ reversed
//
// Funds become spendable only when the provider confirms success through an
// authenticated channel (status query). Callbacks are hints that trigger
// confirmation; a client-reported result is never trusted.
package deposits

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/audit"
	"github.com/sovereigntrust/wallet/internal/auth"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/idempotency"
	"github.com/sovereigntrust/wallet/internal/ledger"
	"github.com/sovereigntrust/wallet/internal/money"
	"github.com/sovereigntrust/wallet/internal/notify"
	"github.com/sovereigntrust/wallet/internal/outbox"
	"github.com/sovereigntrust/wallet/internal/payments"
	"github.com/sovereigntrust/wallet/internal/phone"
	"github.com/sovereigntrust/wallet/internal/policy"
	"github.com/sovereigntrust/wallet/internal/wallet"
)

const (
	JobDispatch = "deposit.dispatch"
	JobPoll     = "deposit.poll"
	// LateWindow keeps polling after expiry to catch late confirmations.
	LateWindow = 30 * time.Minute
)

var (
	depositOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wallet_deposits_total", Help: "Deposit state transitions by provider and outcome.",
	}, []string{"provider", "outcome"})
	providerCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wallet_provider_calls_total", Help: "Provider API calls by provider, operation and result class.",
	}, []string{"provider", "operation", "result"})
)

func init() { prometheus.MustRegister(depositOutcomes, providerCalls) }

// ProviderCallMetric records a provider call outcome (shared with withdrawals).
func ProviderCallMetric(provider, op string, err error) {
	result := "ok"
	switch {
	case err == nil:
	case errors.Is(err, payments.ErrRejected):
		result = "rejected"
	case errors.Is(err, payments.ErrNotSent):
		result = "not_sent"
	case errors.Is(err, payments.ErrUncertain):
		result = "uncertain"
	default:
		result = "error"
	}
	providerCalls.WithLabelValues(provider, op, result).Inc()
}

type Deposit struct {
	ID                  uuid.UUID  `json:"id"`
	UserID              uuid.UUID  `json:"-"`
	WalletID            uuid.UUID  `json:"wallet_id"`
	Provider            string     `json:"provider"`
	ProviderEnv         string     `json:"provider_environment"`
	MSISDN              string     `json:"-"`
	Amount              int64      `json:"-"`
	Currency            string     `json:"currency"`
	Status              string     `json:"status"`
	ProviderStatus      string     `json:"provider_status"`
	ProviderRequestRef  *string    `json:"-"`
	ProviderCheckoutRef *string    `json:"-"`
	ProviderMerchantRef *string    `json:"-"`
	ProviderReceipt     *string    `json:"provider_receipt"`
	ProviderResultCode  *string    `json:"-"`
	ProviderResultDesc  *string    `json:"-"`
	FailureReason       *string    `json:"failure_reason"`
	JournalID           *uuid.UUID `json:"-"`
	NeedsReview         bool       `json:"-"`
	ExpiresAt           time.Time  `json:"expires_at"`
	DispatchedAt        *time.Time `json:"-"`
	CompletedAt         *time.Time `json:"completed_at"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

const cols = `id, user_id, wallet_id, provider, provider_env, msisdn, amount, currency, status, provider_status,
	provider_request_ref, provider_checkout_ref, provider_merchant_ref, provider_receipt, provider_result_code,
	provider_result_desc, failure_reason, journal_id, needs_review, expires_at, dispatched_at, completed_at, created_at, updated_at`

func scan(row pgx.Row) (*Deposit, error) {
	var d Deposit
	err := row.Scan(&d.ID, &d.UserID, &d.WalletID, &d.Provider, &d.ProviderEnv, &d.MSISDN, &d.Amount, &d.Currency, &d.Status,
		&d.ProviderStatus, &d.ProviderRequestRef, &d.ProviderCheckoutRef, &d.ProviderMerchantRef, &d.ProviderReceipt,
		&d.ProviderResultCode, &d.ProviderResultDesc, &d.FailureReason, &d.JournalID, &d.NeedsReview, &d.ExpiresAt,
		&d.DispatchedAt, &d.CompletedAt, &d.CreatedAt, &d.UpdatedAt)
	return &d, err
}

func Get(ctx context.Context, q db.Querier, id uuid.UUID) (*Deposit, error) {
	d, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM deposits WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("deposit")
	}
	return d, err
}

func GetForUser(ctx context.Context, q db.Querier, id, userID uuid.UUID) (*Deposit, error) {
	d, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM deposits WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("deposit")
	}
	return d, err
}

func lock(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Deposit, error) {
	d, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM deposits WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("deposit")
	}
	return d, err
}

var transitions = map[string][]string{
	"created":          {"pending_provider", "failed"},
	"pending_provider": {"succeeded", "failed", "expired"},
	"expired":          {"succeeded", "failed"},
	"succeeded":        {"reversed"},
}

// CanTransition reports whether the deposit state machine allows from -> to.
func CanTransition(from, to string) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

func IsTerminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "reversed"
}

type Service struct {
	Pool                     *pgxpool.Pool
	Providers                payments.Registry
	Idem                     *idempotency.Store
	RequirePhoneVerification bool
	Expiry                   time.Duration
	// CallbackURL builds the provider callback URL for a deposit; empty in simulated mode.
	CallbackURL func(provider string) string
	Log         *slog.Logger
}

type CreateInput struct {
	UserID         uuid.UUID
	Provider       string
	Phone          string
	Amount         string
	Currency       string
	IdempotencyKey string
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*Deposit, bool, error) {
	if in.Currency == "" {
		in.Currency = money.KES.Code
	}
	prov, ok := s.Providers.Get(in.Provider)
	if !ok || !prov.Capabilities().Collection {
		return nil, false, apperr.Field("provider", "unsupported or unavailable payment provider")
	}
	cur, err := money.LookupCurrency(in.Currency)
	if err != nil {
		return nil, false, apperr.Field("currency", "unsupported currency")
	}
	amount, err := money.ParsePositive(in.Amount, cur)
	if err != nil {
		return nil, false, apperr.Field("amount", err.Error())
	}
	if prov.Capabilities().WholeUnitsOnly && !money.IsWholeMajor(amount, cur) {
		return nil, false, apperr.Field("amount", "mobile money deposits must be whole shillings")
	}
	msisdn, err := phone.NormalizeKE(in.Phone)
	if err != nil {
		return nil, false, apperr.Field("phone", "must be a valid Kenyan mobile number")
	}
	key := &idempotency.Key{Principal: in.UserID, Operation: "deposit.create", Key: in.IdempotencyKey,
		Hash: idempotency.Fingerprint([]any{in.Provider, msisdn, amount, cur.Code})}
	prior, err := s.Idem.Acquire(ctx, key)
	if err != nil {
		return nil, false, err
	}
	if prior != nil {
		d, err := Get(ctx, s.Pool, prior.ResourceID)
		return d, true, err
	}
	committed := false
	defer func() {
		if !committed {
			s.Idem.Release(ctx, key)
		}
	}()

	w, err := wallet.PrimaryForUser(ctx, s.Pool, in.UserID, cur.Code)
	if err != nil {
		return nil, false, err
	}
	id := uuid.Must(uuid.NewV7())
	ref := strings.ToUpper(strings.ReplaceAll(id.String(), "-", ""))
	err = db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		lw, err := wallet.LockForUpdate(ctx, tx, w.ID)
		if err != nil {
			return err
		}
		if lw.Status != "active" {
			return apperr.New(apperr.CodeAccountRestricted, "your wallet is "+lw.Status)
		}
		u, err := auth.GetUser(ctx, tx, in.UserID)
		if err != nil {
			return err
		}
		if err := u.CanDeposit(s.RequirePhoneVerification); err != nil {
			return err
		}
		if err := policy.CheckLimits(ctx, tx, in.UserID, u.KYCTier, "deposit", cur.Code, amount); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO deposits (id, user_id, wallet_id, provider, provider_env, msisdn, amount, currency, status,
			                      provider_request_ref, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'created', $9, now() + $10::interval)`,
			id, in.UserID, w.ID, prov.Name(), prov.Environment(), msisdn, amount, cur.Code, ref,
			fmt.Sprintf("%d seconds", int(s.Expiry.Seconds())))
		if err != nil {
			return err
		}
		if err := outbox.Enqueue(ctx, tx, JobDispatch, map[string]string{"deposit_id": id.String()},
			outbox.EnqueueOpts{DedupeKey: JobDispatch + ":" + id.String(), MaxAttempts: 5}); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{Action: "deposit.created", TargetType: "deposit", TargetID: id.String(),
			Details: map[string]any{"provider": prov.Name(), "amount": amount, "env": prov.Environment()}}); err != nil {
			return err
		}
		return s.Idem.CompleteTx(ctx, tx, key, "deposit", id)
	})
	if err != nil {
		return nil, false, err
	}
	committed = true
	depositOutcomes.WithLabelValues(prov.Name(), "created").Inc()
	d, err := Get(ctx, s.Pool, id)
	return d, false, err
}

func (s *Service) schedulePoll(ctx context.Context, q db.Querier, id uuid.UUID, after time.Duration) error {
	// A fresh dedupe key per schedule keeps at most one live poll chain: the
	// poll handler reschedules itself instead of enqueuing new jobs.
	return outbox.Enqueue(ctx, q, JobPoll, map[string]string{"deposit_id": id.String()},
		outbox.EnqueueOpts{DedupeKey: JobPoll + ":" + id.String(), RunAt: time.Now().Add(after), MaxAttempts: 1000})
}

// Dispatch sends the collection request to the provider. Idempotent: only a
// deposit in created/not_sent is ever sent, and the "sending" marker is
// committed before the network call so a crash can never cause a resend.
func (s *Service) Dispatch(ctx context.Context, id uuid.UUID) error {
	var d *Deposit
	var send bool
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		d, err = lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if d.Status != "created" {
			return nil
		}
		if d.ProviderStatus == "sending" {
			// Previous attempt crashed mid-call: outcome unknown, never resend.
			if _, err := tx.Exec(ctx, `UPDATE deposits SET status = 'pending_provider', provider_status = 'unknown', updated_at = now() WHERE id = $1`, id); err != nil {
				return err
			}
			return s.schedulePoll(ctx, tx, id, 5*time.Second)
		}
		if time.Now().After(d.ExpiresAt) {
			return s.failTx(ctx, tx, d, "rejected", "", "deposit expired before it could be sent to the provider")
		}
		send = true
		_, err = tx.Exec(ctx, `UPDATE deposits SET provider_status = 'sending', dispatched_at = now(), updated_at = now() WHERE id = $1`, id)
		return err
	})
	if err != nil || !send {
		return err
	}
	prov, ok := s.Providers.Get(d.Provider)
	if !ok {
		return outbox.Permanent(fmt.Errorf("provider %s not configured", d.Provider))
	}
	cbURL := ""
	if s.CallbackURL != nil {
		cbURL = s.CallbackURL(d.Provider)
	}
	resp, callErr := prov.InitiateCollection(ctx, payments.CollectionRequest{
		Reference: *d.ProviderRequestRef, MSISDN: d.MSISDN, Amount: d.Amount, Currency: d.Currency,
		Description: "Wallet deposit", CallbackURL: cbURL,
	})
	ProviderCallMetric(d.Provider, "collection.initiate", callErr)

	var retryErr error
	err = db.WithTx(context.WithoutCancel(ctx), s.Pool, func(tx pgx.Tx) error {
		d, err := lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if d.Status != "created" || d.ProviderStatus != "sending" {
			return nil
		}
		var perr *payments.ProviderError
		errors.As(callErr, &perr)
		switch {
		case callErr == nil:
			_, err = tx.Exec(ctx, `
				UPDATE deposits SET status = 'pending_provider', provider_status = 'accepted', provider_checkout_ref = $2,
				       provider_merchant_ref = NULLIF($3, ''), provider_result_code = $4, provider_result_desc = $5, updated_at = now()
				 WHERE id = $1`, id, resp.CheckoutRef, resp.MerchantRef, resp.ResultCode, resp.ResultDesc)
			if err != nil {
				return err
			}
			return s.schedulePoll(ctx, tx, id, 5*time.Second)
		case errors.Is(callErr, payments.ErrRejected):
			code, desc := "", callErr.Error()
			if perr != nil {
				code, desc = perr.Code, perr.Message
			}
			return s.failTx(ctx, tx, d, "rejected", code, "provider rejected the request: "+desc)
		case errors.Is(callErr, payments.ErrNotSent):
			// Safe to retry: the request never reached the provider.
			retryErr = fmt.Errorf("provider not reachable, will retry: %w", callErr)
			_, err := tx.Exec(ctx, `UPDATE deposits SET provider_status = 'not_sent', updated_at = now() WHERE id = $1`, id)
			return err
		default: // uncertain
			s.Log.Warn("deposit dispatch outcome uncertain", "deposit_id", id, "err", callErr)
			if _, err := tx.Exec(ctx, `UPDATE deposits SET status = 'pending_provider', provider_status = 'unknown', updated_at = now() WHERE id = $1`, id); err != nil {
				return err
			}
			return s.schedulePoll(ctx, tx, id, 10*time.Second)
		}
	})
	if err != nil {
		return err
	}
	return retryErr
}

func (s *Service) failTx(ctx context.Context, tx pgx.Tx, d *Deposit, providerStatus, code, reason string) error {
	if !CanTransition(d.Status, "failed") {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE deposits SET status = 'failed', provider_status = $2, provider_result_code = NULLIF($3, ''),
		       failure_reason = $4, completed_at = now(), updated_at = now()
		 WHERE id = $1`, d.ID, providerStatus, code, reason)
	if err != nil {
		return err
	}
	depositOutcomes.WithLabelValues(d.Provider, "failed").Inc()
	cur, _ := money.LookupCurrency(d.Currency)
	return notify.Create(ctx, tx, d.UserID, "transaction", "Deposit not completed",
		fmt.Sprintf("Your deposit of %s %s was not completed. No money was added.", d.Currency, money.Format(d.Amount, cur)),
		map[string]any{"deposit_id": d.ID}, "deposit:"+d.ID.String()+":failed")
}

// Poll confirms a deposit's outcome with the provider's authenticated status
// API. It returns outbox.RetryAfter while the outcome is still open.
func (s *Service) Poll(ctx context.Context, id uuid.UUID) error {
	d, err := Get(ctx, s.Pool, id)
	if err != nil {
		return outbox.Permanent(err)
	}
	if IsTerminal(d.Status) {
		return nil
	}
	if d.Status == "created" {
		return outbox.RetryAfter(10*time.Second, "not dispatched yet")
	}
	prov, ok := s.Providers.Get(d.Provider)
	if !ok {
		return outbox.Permanent(fmt.Errorf("provider %s not configured", d.Provider))
	}
	now := time.Now()
	if !prov.Capabilities().CollectionStatusQuery {
		return s.expireIfDue(ctx, d, now, "provider has no status query")
	}
	res, qerr := prov.QueryCollection(ctx, payments.StatusQuery{RequestRef: deref(d.ProviderRequestRef), CheckoutRef: deref(d.ProviderCheckoutRef), MSISDN: d.MSISDN, Amount: d.Amount})
	ProviderCallMetric(d.Provider, "collection.query", qerr)
	if qerr != nil {
		s.Log.Warn("deposit status query failed", "deposit_id", id, "err", qerr)
		return s.expireIfDue(ctx, d, now, "status query failing")
	}
	switch res.Outcome {
	case payments.OutcomeSucceeded:
		return s.Settle(ctx, id, res.Receipt, res.ResultCode, res.ResultDesc)
	case payments.OutcomeFailed:
		return db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
			d, err := lock(ctx, tx, id)
			if err != nil {
				return err
			}
			reason := res.ResultDesc
			if reason == "" {
				reason = "payment was not completed"
			}
			return s.failTx(ctx, tx, d, "failed", res.ResultCode, reason)
		})
	default:
		return s.expireIfDue(ctx, d, now, string(res.Outcome))
	}
}

// expireIfDue marks an open deposit expired once past its window and keeps
// polling through the late window; afterwards it stops and leaves the
// deposit flagged for reconciliation.
func (s *Service) expireIfDue(ctx context.Context, d *Deposit, now time.Time, why string) error {
	if now.Before(d.ExpiresAt) {
		return outbox.RetryAfter(pollInterval(now.Sub(d.CreatedAt)), why)
	}
	if d.Status == "pending_provider" {
		err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
			cur, err := lock(ctx, tx, d.ID)
			if err != nil || cur.Status != "pending_provider" {
				return err
			}
			_, err = tx.Exec(ctx, `
				UPDATE deposits SET status = 'expired', needs_review = true,
				       failure_reason = 'no confirmation received from provider in time', updated_at = now()
				 WHERE id = $1`, d.ID)
			if err != nil {
				return err
			}
			depositOutcomes.WithLabelValues(d.Provider, "expired").Inc()
			return notify.Create(ctx, tx, d.UserID, "transaction", "Deposit pending confirmation",
				"We have not received confirmation of your deposit yet. If money left your mobile wallet, it will be credited once confirmed.",
				map[string]any{"deposit_id": d.ID}, "deposit:"+d.ID.String()+":expired")
		})
		if err != nil {
			return err
		}
	}
	if now.After(d.ExpiresAt.Add(LateWindow)) {
		return nil // stop polling; reconciliation owns it now
	}
	return outbox.RetryAfter(time.Minute, why+" (late window)")
}

func pollInterval(age time.Duration) time.Duration {
	switch {
	case age < time.Minute:
		return 5 * time.Second
	case age < 5*time.Minute:
		return 15 * time.Second
	default:
		return time.Minute
	}
}

// Settle credits the wallet exactly once for a confirmed deposit.
func (s *Service) Settle(ctx context.Context, id uuid.UUID, receipt, code, desc string) error {
	if receipt == "" {
		// Daraja's STK query carries no receipt; recover it from a stored callback.
		receipt = s.callbackReceipt(ctx, id)
	}
	return db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		d, err := lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if d.Status == "succeeded" || d.Status == "reversed" {
			return nil
		}
		if !CanTransition(d.Status, "succeeded") {
			s.Log.Error("deposit confirmed by provider but state forbids success; flagging", "deposit_id", id, "status", d.Status)
			_, err := tx.Exec(ctx, `UPDATE deposits SET needs_review = true, updated_at = now() WHERE id = $1`, id)
			return err
		}
		needsReview := d.Status == "expired" || d.NeedsReview
		var receiptArg *string
		if receipt != "" {
			var dup bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deposits WHERE provider = $1 AND provider_receipt = $2 AND id <> $3)`,
				d.Provider, receipt, id).Scan(&dup); err != nil {
				return err
			}
			if dup {
				needsReview = true
			} else {
				receiptArg = &receipt
			}
		}
		clearing, err := ledger.ProviderClearingAccount(d.Provider, d.Currency)
		if err != nil {
			return err
		}
		w, err := wallet.Get(ctx, tx, d.WalletID)
		if err != nil {
			return err
		}
		jid, err := ledger.Post(ctx, tx, ledger.Journal{
			PostingKey: "deposit:" + id.String() + ":settle", Kind: "deposit", ReferenceType: "deposit", ReferenceID: id,
			Description: d.Provider + " deposit", Currency: d.Currency,
			Entries: []ledger.Entry{
				{AccountID: clearing, Direction: ledger.Debit, Amount: d.Amount},
				{AccountID: w.AvailableAccountID, Direction: ledger.Credit, Amount: d.Amount},
			},
		})
		if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE deposits SET status = 'succeeded', provider_status = 'completed', journal_id = $2,
			       provider_receipt = coalesce(provider_receipt, $3), provider_result_code = NULLIF($4, ''),
			       provider_result_desc = NULLIF($5, ''), needs_review = $6, completed_at = now(), updated_at = now()
			 WHERE id = $1`, id, jid, receiptArg, code, desc, needsReview)
		if err != nil {
			return err
		}
		depositOutcomes.WithLabelValues(d.Provider, "succeeded").Inc()
		cur, _ := money.LookupCurrency(d.Currency)
		return notify.Create(ctx, tx, d.UserID, "transaction", "Deposit received",
			fmt.Sprintf("%s %s has been added to your wallet.", d.Currency, money.Format(d.Amount, cur)),
			map[string]any{"deposit_id": id}, "deposit:"+id.String()+":succeeded")
	})
}

// callbackReceipt finds the receipt from a stored success callback for this
// deposit (used only as a label; the outcome itself came from the query).
func (s *Service) callbackReceipt(ctx context.Context, id uuid.UUID) string {
	var receipt *string
	_ = s.Pool.QueryRow(ctx, `
		SELECT jsonb_path_query_first(c.payload,
		         '$.Body.stkCallback.CallbackMetadata.Item[*] ? (@.Name == "MpesaReceiptNumber").Value') #>> '{}'
		  FROM provider_callbacks c
		  JOIN deposits d ON d.provider_checkout_ref = c.payload #>> '{Body,stkCallback,CheckoutRequestID}'
		 WHERE d.id = $1 AND c.provider = 'mpesa' AND c.kind = 'stk'
		   AND c.payload #>> '{Body,stkCallback,ResultCode}' = '0'
		 ORDER BY c.received_at DESC LIMIT 1`, id).Scan(&receipt)
	if receipt == nil || len(*receipt) > 32 {
		return ""
	}
	return *receipt
}

// FindByCheckoutRef locates a deposit from a callback reference.
func FindByCheckoutRef(ctx context.Context, q db.Querier, provider, ref string) (*Deposit, error) {
	d, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM deposits WHERE provider = $1 AND provider_checkout_ref = $2`, provider, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("deposit")
	}
	return d, err
}

func FindByRequestRef(ctx context.Context, q db.Querier, provider, ref string) (*Deposit, error) {
	d, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM deposits WHERE provider = $1 AND provider_request_ref = $2`, provider, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("deposit")
	}
	return d, err
}

// RequestRecheck schedules an immediate provider status check (admin tool or
// callback trigger). It never changes state by itself.
func (s *Service) RequestRecheck(ctx context.Context, q db.Querier, id uuid.UUID) error {
	_, err := q.Exec(ctx, `
		INSERT INTO outbox_jobs (kind, payload, dedupe_key, run_at, max_attempts)
		VALUES ($1, jsonb_build_object('deposit_id', $2::text), $3, now(), 1000)
		ON CONFLICT (dedupe_key) DO UPDATE SET run_at = now(), status = 'pending', updated_at = now()
		 WHERE outbox_jobs.status IN ('pending', 'done', 'dead')`,
		JobPoll, id.String(), JobPoll+":"+id.String())
	return err
}

// Sweep fails deposits that were never sent before expiry and makes sure
// every open deposit has a poll job. Run periodically by the worker.
func (s *Service) Sweep(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `
		SELECT id FROM deposits WHERE status = 'created' AND provider_status = 'not_sent' AND expires_at < now() - interval '1 minute'
		LIMIT 200`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
			d, err := lock(ctx, tx, id)
			if err != nil || d.Status != "created" || d.ProviderStatus != "not_sent" {
				return err
			}
			return s.failTx(ctx, tx, d, "rejected", "", "could not reach the payment provider")
		})
		if err != nil {
			s.Log.Error("sweep deposit", "deposit_id", id, "err", err)
		}
	}
	_, err = s.Pool.Exec(ctx, `
		INSERT INTO outbox_jobs (kind, payload, dedupe_key, max_attempts)
		SELECT $1, jsonb_build_object('deposit_id', d.id::text), $1 || ':' || d.id::text, 1000
		  FROM deposits d
		 WHERE d.status = 'pending_provider'
		   AND d.updated_at < now() - interval '2 minutes'
		ON CONFLICT (dedupe_key) DO NOTHING`, JobPoll)
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Confirm is triggered by a provider callback. It asks the provider's
// authenticated status API for the outcome and applies it; the callback
// content itself is only used for the receipt label. If the provider cannot
// confirm yet, the regular poll chain continues.
func (s *Service) Confirm(ctx context.Context, id uuid.UUID, hintReceipt string) (string, error) {
	d, err := Get(ctx, s.Pool, id)
	if err != nil {
		return "", err
	}
	if IsTerminal(d.Status) {
		return "already " + d.Status, nil
	}
	prov, ok := s.Providers.Get(d.Provider)
	if !ok || !prov.Capabilities().CollectionStatusQuery {
		return "no status query available; left for polling/reconciliation", s.RequestRecheck(ctx, s.Pool, id)
	}
	res, qerr := prov.QueryCollection(ctx, payments.StatusQuery{RequestRef: deref(d.ProviderRequestRef), CheckoutRef: deref(d.ProviderCheckoutRef), MSISDN: d.MSISDN, Amount: d.Amount})
	ProviderCallMetric(d.Provider, "collection.query", qerr)
	if qerr != nil {
		return "status query failed; polling continues", s.RequestRecheck(ctx, s.Pool, id)
	}
	switch res.Outcome {
	case payments.OutcomeSucceeded:
		receipt := res.Receipt
		if receipt == "" {
			receipt = hintReceipt
		}
		return "confirmed succeeded", s.Settle(ctx, id, receipt, res.ResultCode, res.ResultDesc)
	case payments.OutcomeFailed:
		return "confirmed failed", db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
			d, err := lock(ctx, tx, id)
			if err != nil {
				return err
			}
			return s.failTx(ctx, tx, d, "failed", res.ResultCode, res.ResultDesc)
		})
	}
	return "provider reports " + string(res.Outcome) + "; polling continues", s.RequestRecheck(ctx, s.Pool, id)
}

// FlagReview marks a deposit for manual review (e.g. inconsistent callback).
func FlagReview(ctx context.Context, q db.Querier, id uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE deposits SET needs_review = true, updated_at = now() WHERE id = $1`, id)
	return err
}

// Reverse books a provider-side reversal of a succeeded deposit (approved
// maker-checker workflow only). The customer must still hold the funds; the
// database rejects the posting otherwise.
func Reverse(ctx context.Context, tx pgx.Tx, id, actor uuid.UUID) (uuid.UUID, error) {
	d, err := lock(ctx, tx, id)
	if err != nil {
		return uuid.Nil, err
	}
	if d.Status != "succeeded" || d.JournalID == nil {
		return uuid.Nil, apperr.New(apperr.CodeInvalidState, "only succeeded deposits can be reversed")
	}
	jid, err := ledger.Reverse(ctx, tx, *d.JournalID, "deposit_reversal", "deposit:"+id.String()+":reversal", "Deposit reversed", &actor)
	if errors.Is(err, ledger.ErrInsufficientFunds) {
		return uuid.Nil, apperr.New(apperr.CodeInsufficientFunds, "customer no longer holds the deposited funds; reversal cannot be posted")
	}
	if err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE deposits SET status = 'reversed', reversal_journal_id = $2, updated_at = now() WHERE id = $1`, id, jid)
	return jid, err
}
