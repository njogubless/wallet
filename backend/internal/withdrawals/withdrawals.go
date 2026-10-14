// Package withdrawals implements payouts from wallets to mobile money.
//
// Accounting model (reservation):
//
//	request   Dr wallet_available (amount+fee)  Cr wallet_hold (amount+fee)
//	success   Dr wallet_hold (amount+fee)       Cr provider_clearing (amount), Cr fee_revenue (fee)
//	failure   Dr wallet_hold (amount+fee)       Cr wallet_available (amount+fee)
//
// Status: created → pending (funds held) → processing (sent / being sent)
// → succeeded | failed; succeeded → reversed (approved reversal only).
//
// Funds are released only on a definitive failure from an authenticated
// source (provider rejected the request, a status query reports failure, or
// a matched result callback reports failure). Timeouts and ambiguous
// responses mark the withdrawal outcome_uncertain and leave funds held until
// confirmation, reconciliation or an audited admin resolution. A payout is
// never re-sent after its "sending" marker has been committed.
package withdrawals

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
	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/idempotency"
	"github.com/sovereigntrust/wallet/internal/ledger"
	"github.com/sovereigntrust/wallet/internal/money"
	"github.com/sovereigntrust/wallet/internal/notify"
	"github.com/sovereigntrust/wallet/internal/outbox"
	"github.com/sovereigntrust/wallet/internal/payments"
	"github.com/sovereigntrust/wallet/internal/policy"
	"github.com/sovereigntrust/wallet/internal/wallet"
)

const (
	JobDispatch = "withdrawal.dispatch"
	JobPoll     = "withdrawal.poll"
	// ResultWatchdog: without a status API, flag for review if no result arrives.
	ResultWatchdog = 10 * time.Minute
	// PollHorizon: stop automatic polling after this long; reconciliation owns it.
	PollHorizon = 24 * time.Hour
)

var withdrawalOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "wallet_withdrawals_total", Help: "Withdrawal state transitions by provider and outcome.",
}, []string{"provider", "outcome"})

func init() { prometheus.MustRegister(withdrawalOutcomes) }

type Withdrawal struct {
	ID                 uuid.UUID  `json:"id"`
	UserID             uuid.UUID  `json:"-"`
	WalletID           uuid.UUID  `json:"wallet_id"`
	Provider           string     `json:"provider"`
	ProviderEnv        string     `json:"provider_environment"`
	MSISDN             string     `json:"-"`
	Amount             int64      `json:"-"`
	Fee                int64      `json:"-"`
	Currency           string     `json:"currency"`
	Status             string     `json:"status"`
	ProviderStatus     string     `json:"provider_status"`
	OutcomeUncertain   bool       `json:"outcome_uncertain"`
	ProviderRequestRef *string    `json:"-"`
	ProviderConvRef    *string    `json:"-"`
	ProviderReceipt    *string    `json:"provider_receipt"`
	ProviderResultCode *string    `json:"-"`
	ProviderResultDesc *string    `json:"-"`
	FailureReason      *string    `json:"failure_reason"`
	HoldJournalID      *uuid.UUID `json:"-"`
	SettleJournalID    *uuid.UUID `json:"-"`
	ReleaseJournalID   *uuid.UUID `json:"-"`
	NeedsReview        bool       `json:"-"`
	DispatchStartedAt  *time.Time `json:"-"`
	CompletedAt        *time.Time `json:"completed_at"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func (w *Withdrawal) Total() int64 { return w.Amount + w.Fee }

const cols = `id, user_id, wallet_id, provider, provider_env, msisdn, amount, fee, currency, status, provider_status,
	outcome_uncertain, provider_request_ref, provider_conversation_ref, provider_receipt, provider_result_code,
	provider_result_desc, failure_reason, hold_journal_id, settle_journal_id, release_journal_id, needs_review,
	dispatch_started_at, completed_at, created_at, updated_at`

func scan(row pgx.Row) (*Withdrawal, error) {
	var w Withdrawal
	err := row.Scan(&w.ID, &w.UserID, &w.WalletID, &w.Provider, &w.ProviderEnv, &w.MSISDN, &w.Amount, &w.Fee, &w.Currency,
		&w.Status, &w.ProviderStatus, &w.OutcomeUncertain, &w.ProviderRequestRef, &w.ProviderConvRef, &w.ProviderReceipt,
		&w.ProviderResultCode, &w.ProviderResultDesc, &w.FailureReason, &w.HoldJournalID, &w.SettleJournalID,
		&w.ReleaseJournalID, &w.NeedsReview, &w.DispatchStartedAt, &w.CompletedAt, &w.CreatedAt, &w.UpdatedAt)
	return &w, err
}

func Get(ctx context.Context, q db.Querier, id uuid.UUID) (*Withdrawal, error) {
	w, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM withdrawals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("withdrawal")
	}
	return w, err
}

func GetForUser(ctx context.Context, q db.Querier, id, userID uuid.UUID) (*Withdrawal, error) {
	w, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM withdrawals WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("withdrawal")
	}
	return w, err
}

func lock(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Withdrawal, error) {
	w, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM withdrawals WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("withdrawal")
	}
	return w, err
}

func FindByRequestRef(ctx context.Context, q db.Querier, provider, ref string) (*Withdrawal, error) {
	w, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM withdrawals WHERE provider = $1 AND provider_request_ref = $2`, provider, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("withdrawal")
	}
	return w, err
}

func FindByConversationRef(ctx context.Context, q db.Querier, provider, ref string) (*Withdrawal, error) {
	w, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM withdrawals WHERE provider = $1 AND provider_conversation_ref = $2`, provider, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("withdrawal")
	}
	return w, err
}

var transitions = map[string][]string{
	"created":    {"pending", "failed"},
	"pending":    {"processing", "failed"},
	"processing": {"succeeded", "failed", "pending"}, // -> pending only when the request provably never left
	"succeeded":  {"reversed"},
}

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
	Auth                     *auth.Service
	RequirePhoneVerification bool
	// ResultURLs returns (resultURL, timeoutURL) for payout callbacks.
	ResultURLs func(provider string) (string, string)
	Log        *slog.Logger
}

type CreateInput struct {
	UserID         uuid.UUID
	Provider       string
	Amount         string
	Currency       string
	PIN            string
	IdempotencyKey string
}

type Quote struct {
	Amount, Fee, Total int64
	Currency           money.Currency
}

func (s *Service) Preview(ctx context.Context, provider, amountStr, currency string) (Quote, error) {
	prov, ok := s.Providers.Get(provider)
	if !ok || !prov.Capabilities().Payout {
		return Quote{}, apperr.Field("provider", "withdrawals are not available for this provider")
	}
	cur, err := money.LookupCurrency(currency)
	if err != nil {
		return Quote{}, apperr.Field("currency", "unsupported currency")
	}
	amount, err := money.ParsePositive(amountStr, cur)
	if err != nil {
		return Quote{}, apperr.Field("amount", err.Error())
	}
	if prov.Capabilities().WholeUnitsOnly && !money.IsWholeMajor(amount, cur) {
		return Quote{}, apperr.Field("amount", "mobile money withdrawals must be whole shillings")
	}
	fee, err := policy.Fee(ctx, s.Pool, "withdrawal", provider, cur.Code, amount)
	if err != nil {
		return Quote{}, err
	}
	total, err := money.Add(amount, fee)
	if err != nil {
		return Quote{}, apperr.Field("amount", "amount too large")
	}
	return Quote{Amount: amount, Fee: fee, Total: total, Currency: cur}, nil
}

// Create holds funds and queues the payout. Payouts go only to the account
// holder's own registered phone number.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Withdrawal, bool, error) {
	if in.Currency == "" {
		in.Currency = money.KES.Code
	}
	q, err := s.Preview(ctx, in.Provider, in.Amount, in.Currency)
	if err != nil {
		return nil, false, err
	}
	prov, _ := s.Providers.Get(in.Provider)
	key := &idempotency.Key{Principal: in.UserID, Operation: "withdrawal.create", Key: in.IdempotencyKey,
		Hash: idempotency.Fingerprint([]any{in.Provider, q.Amount, q.Currency.Code})}
	prior, err := s.Idem.Acquire(ctx, key)
	if err != nil {
		return nil, false, err
	}
	if prior != nil {
		w, err := Get(ctx, s.Pool, prior.ResourceID)
		return w, true, err
	}
	committed := false
	defer func() {
		if !committed {
			s.Idem.Release(ctx, key)
		}
	}()
	if err := s.Auth.VerifyPIN(ctx, in.UserID, in.PIN); err != nil {
		return nil, false, err
	}
	wl, err := wallet.PrimaryForUser(ctx, s.Pool, in.UserID, q.Currency.Code)
	if err != nil {
		return nil, false, err
	}
	id := uuid.Must(uuid.NewV7())
	ref := strings.ToUpper(strings.ReplaceAll(id.String(), "-", ""))
	err = db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		w, err := wallet.LockForUpdate(ctx, tx, wl.ID)
		if err != nil {
			return err
		}
		if w.Status != "active" {
			return apperr.New(apperr.CodeAccountRestricted, "your wallet is "+w.Status)
		}
		u, err := auth.GetUser(ctx, tx, in.UserID)
		if err != nil {
			return err
		}
		if err := u.CanSend(s.RequirePhoneVerification); err != nil {
			return err
		}
		if err := policy.CheckLimits(ctx, tx, in.UserID, u.KYCTier, "withdrawal", q.Currency.Code, q.Amount); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO withdrawals (id, user_id, wallet_id, provider, provider_env, msisdn, amount, fee, currency, status, provider_request_ref)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'created', $10)`,
			id, in.UserID, w.ID, prov.Name(), prov.Environment(), u.Phone, q.Amount, q.Fee, q.Currency.Code, ref); err != nil {
			return err
		}
		jid, err := ledger.Post(ctx, tx, ledger.Journal{
			PostingKey: "withdrawal:" + id.String() + ":hold", Kind: "withdrawal_hold", ReferenceType: "withdrawal", ReferenceID: id,
			Description: "Withdrawal hold", CreatedBy: &in.UserID, Currency: q.Currency.Code,
			Entries: []ledger.Entry{
				{AccountID: w.AvailableAccountID, Direction: ledger.Debit, Amount: q.Total},
				{AccountID: w.HoldAccountID, Direction: ledger.Credit, Amount: q.Total},
			},
		})
		if errors.Is(err, ledger.ErrInsufficientFunds) {
			return apperr.ErrInsufficientFunds
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE withdrawals SET status = 'pending', hold_journal_id = $2, updated_at = now() WHERE id = $1`, id, jid); err != nil {
			return err
		}
		if err := outbox.Enqueue(ctx, tx, JobDispatch, map[string]string{"withdrawal_id": id.String()},
			outbox.EnqueueOpts{DedupeKey: JobDispatch + ":" + id.String(), MaxAttempts: 8}); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{Action: "withdrawal.created", TargetType: "withdrawal", TargetID: id.String(),
			Details: map[string]any{"provider": prov.Name(), "amount": q.Amount, "fee": q.Fee, "env": prov.Environment()}}); err != nil {
			return err
		}
		return s.Idem.CompleteTx(ctx, tx, key, "withdrawal", id)
	})
	if err != nil {
		return nil, false, err
	}
	committed = true
	withdrawalOutcomes.WithLabelValues(prov.Name(), "created").Inc()
	w, err := Get(ctx, s.Pool, id)
	return w, false, err
}

func (s *Service) schedulePoll(ctx context.Context, q db.Querier, id uuid.UUID, after time.Duration) error {
	return outbox.Enqueue(ctx, q, JobPoll, map[string]string{"withdrawal_id": id.String()},
		outbox.EnqueueOpts{DedupeKey: JobPoll + ":" + id.String(), RunAt: time.Now().Add(after), MaxAttempts: 1000})
}

func (s *Service) markUncertainTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, why string) error {
	_, err := tx.Exec(ctx, `
		UPDATE withdrawals SET provider_status = 'unknown', outcome_uncertain = true, needs_review = true,
		       provider_result_desc = left($2, 500), updated_at = now()
		 WHERE id = $1`, id, why)
	return err
}

// Dispatch sends the payout. See package doc for the no-resend guarantee.
func (s *Service) Dispatch(ctx context.Context, id uuid.UUID) error {
	var w *Withdrawal
	var send bool
	prov := payments.Provider(nil)
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		w, err = lock(ctx, tx, id)
		if err != nil {
			return err
		}
		p, ok := s.Providers.Get(w.Provider)
		if !ok {
			return outbox.Permanent(fmt.Errorf("provider %s not configured", w.Provider))
		}
		prov = p
		if w.Status == "processing" && w.ProviderStatus == "sending" {
			// Crashed during a previous send: outcome unknown. Never resend.
			s.Log.Warn("withdrawal found mid-send after restart; marking uncertain", "withdrawal_id", id)
			if err := s.markUncertainTx(ctx, tx, id, "worker interrupted while sending"); err != nil {
				return err
			}
			withdrawalOutcomes.WithLabelValues(w.Provider, "uncertain").Inc()
			return s.schedulePoll(ctx, tx, id, 30*time.Second)
		}
		if w.Status != "pending" {
			return nil
		}
		send = true
		_, err = tx.Exec(ctx, `
			UPDATE withdrawals SET status = 'processing', provider_status = 'sending', dispatch_started_at = now(), updated_at = now()
			 WHERE id = $1`, id)
		return err
	})
	if err != nil || !send {
		return err
	}
	resultURL, timeoutURL := "", ""
	if s.ResultURLs != nil {
		resultURL, timeoutURL = s.ResultURLs(w.Provider)
	}
	resp, callErr := prov.InitiatePayout(ctx, payments.PayoutRequest{
		Reference: *w.ProviderRequestRef, MSISDN: w.MSISDN, Amount: w.Amount, Currency: w.Currency,
		Remarks: "Wallet withdrawal", ResultURL: resultURL, TimeoutURL: timeoutURL,
	})
	deposits.ProviderCallMetric(w.Provider, "payout.initiate", callErr)

	var retryErr error
	err = db.WithTx(context.WithoutCancel(ctx), s.Pool, func(tx pgx.Tx) error {
		w, err := lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if w.Status != "processing" || w.ProviderStatus != "sending" {
			return nil // a callback already resolved it
		}
		var perr *payments.ProviderError
		errors.As(callErr, &perr)
		switch {
		case callErr == nil:
			if _, err := tx.Exec(ctx, `
				UPDATE withdrawals SET provider_status = 'accepted', provider_conversation_ref = NULLIF($2, ''),
				       provider_result_code = $3, provider_result_desc = $4, updated_at = now()
				 WHERE id = $1`, id, resp.ConversationRef, resp.ResultCode, resp.ResultDesc); err != nil {
				return err
			}
			if prov.Capabilities().PayoutStatusQuery {
				return s.schedulePoll(ctx, tx, id, 10*time.Second)
			}
			return s.schedulePoll(ctx, tx, id, ResultWatchdog) // watchdog only
		case errors.Is(callErr, payments.ErrRejected):
			code, desc := "", callErr.Error()
			if perr != nil {
				code, desc = perr.Code, perr.Message
			}
			return s.releaseTx(ctx, tx, w, "rejected", code, "provider rejected the payout: "+desc)
		case errors.Is(callErr, payments.ErrNotSent):
			retryErr = fmt.Errorf("provider not reachable, will retry: %w", callErr)
			_, err := tx.Exec(ctx, `UPDATE withdrawals SET status = 'pending', provider_status = 'not_sent', updated_at = now() WHERE id = $1`, id)
			return err
		default:
			s.Log.Warn("withdrawal dispatch outcome uncertain", "withdrawal_id", id, "err", callErr)
			if err := s.markUncertainTx(ctx, tx, id, callErr.Error()); err != nil {
				return err
			}
			withdrawalOutcomes.WithLabelValues(w.Provider, "uncertain").Inc()
			if prov.Capabilities().PayoutStatusQuery {
				return s.schedulePoll(ctx, tx, id, 30*time.Second)
			}
			return nil
		}
	})
	if err != nil {
		return err
	}
	return retryErr
}

// settleTx books a confirmed payout.
func (s *Service) settleTx(ctx context.Context, tx pgx.Tx, w *Withdrawal, receipt, code, desc string) error {
	if w.Status == "succeeded" || w.Status == "reversed" {
		return nil
	}
	if !CanTransition(w.Status, "succeeded") {
		return apperr.New(apperr.CodeInvalidState, "withdrawal cannot succeed from status "+w.Status)
	}
	wl, err := wallet.Get(ctx, tx, w.WalletID)
	if err != nil {
		return err
	}
	clearing, err := ledger.ProviderClearingAccount(w.Provider, w.Currency)
	if err != nil {
		return err
	}
	entries := []ledger.Entry{
		{AccountID: wl.HoldAccountID, Direction: ledger.Debit, Amount: w.Total()},
		{AccountID: clearing, Direction: ledger.Credit, Amount: w.Amount},
	}
	if w.Fee > 0 {
		feeAcct, err := ledger.FeeRevenueAccount(w.Currency)
		if err != nil {
			return err
		}
		entries = append(entries, ledger.Entry{AccountID: feeAcct, Direction: ledger.Credit, Amount: w.Fee})
	}
	jid, err := ledger.Post(ctx, tx, ledger.Journal{
		PostingKey: "withdrawal:" + w.ID.String() + ":settle", Kind: "withdrawal_settle", ReferenceType: "withdrawal",
		ReferenceID: w.ID, ReversesJournalID: nil, Description: "Withdrawal settled", Currency: w.Currency, Entries: entries,
	})
	if err != nil {
		return err
	}
	var receiptArg *string
	if receipt != "" {
		var dup bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM withdrawals WHERE provider = $1 AND provider_receipt = $2 AND id <> $3)`,
			w.Provider, receipt, w.ID).Scan(&dup); err != nil {
			return err
		}
		if !dup {
			receiptArg = &receipt
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE withdrawals SET status = 'succeeded', provider_status = 'completed', settle_journal_id = $2,
		       provider_receipt = coalesce(provider_receipt, $3), provider_result_code = NULLIF($4, ''),
		       provider_result_desc = NULLIF($5, ''), needs_review = needs_review OR ($3::text IS NULL AND $6),
		       completed_at = now(), updated_at = now()
		 WHERE id = $1`, w.ID, jid, receiptArg, code, desc, receipt != ""); err != nil {
		return err
	}
	withdrawalOutcomes.WithLabelValues(w.Provider, "succeeded").Inc()
	cur, _ := money.LookupCurrency(w.Currency)
	return notify.Create(ctx, tx, w.UserID, "transaction", "Withdrawal sent",
		fmt.Sprintf("%s %s was sent to your mobile money account.", w.Currency, money.Format(w.Amount, cur)),
		map[string]any{"withdrawal_id": w.ID}, "withdrawal:"+w.ID.String()+":succeeded")
}

// releaseTx returns held funds after a definitive failure.
func (s *Service) releaseTx(ctx context.Context, tx pgx.Tx, w *Withdrawal, providerStatus, code, reason string) error {
	if w.Status == "failed" {
		return nil
	}
	if !CanTransition(w.Status, "failed") {
		return apperr.New(apperr.CodeInvalidState, "withdrawal cannot fail from status "+w.Status)
	}
	wl, err := wallet.Get(ctx, tx, w.WalletID)
	if err != nil {
		return err
	}
	jid, err := ledger.Post(ctx, tx, ledger.Journal{
		PostingKey: "withdrawal:" + w.ID.String() + ":release", Kind: "withdrawal_release", ReferenceType: "withdrawal",
		ReferenceID: w.ID, Description: "Withdrawal funds released", Currency: w.Currency,
		Entries: []ledger.Entry{
			{AccountID: wl.HoldAccountID, Direction: ledger.Debit, Amount: w.Total()},
			{AccountID: wl.AvailableAccountID, Direction: ledger.Credit, Amount: w.Total()},
		},
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE withdrawals SET status = 'failed', provider_status = $2, provider_result_code = NULLIF($3, ''),
		       failure_reason = $4, release_journal_id = $5, completed_at = now(), updated_at = now()
		 WHERE id = $1`, w.ID, providerStatus, code, reason, jid); err != nil {
		return err
	}
	withdrawalOutcomes.WithLabelValues(w.Provider, "failed").Inc()
	cur, _ := money.LookupCurrency(w.Currency)
	return notify.Create(ctx, tx, w.UserID, "transaction", "Withdrawal failed",
		fmt.Sprintf("Your withdrawal of %s %s failed and the funds were returned to your wallet.", w.Currency, money.Format(w.Amount, cur)),
		map[string]any{"withdrawal_id": w.ID}, "withdrawal:"+w.ID.String()+":failed")
}

// ApplyOutcome applies a definitive outcome from an authenticated source
// (status query or a matched result callback).
func (s *Service) ApplyOutcome(ctx context.Context, id uuid.UUID, res payments.StatusResult) error {
	return db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		w, err := lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if IsTerminal(w.Status) {
			if (w.Status == "succeeded" && res.Outcome == payments.OutcomeFailed) || (w.Status == "failed" && res.Outcome == payments.OutcomeSucceeded) {
				// Conflicting definitive outcomes: never flip silently.
				s.Log.Error("conflicting withdrawal outcome", "withdrawal_id", id, "status", w.Status, "new", res.Outcome)
				_, err := tx.Exec(ctx, `UPDATE withdrawals SET needs_review = true, updated_at = now() WHERE id = $1`, id)
				return err
			}
			return nil
		}
		switch res.Outcome {
		case payments.OutcomeSucceeded:
			return s.settleTx(ctx, tx, w, res.Receipt, res.ResultCode, res.ResultDesc)
		case payments.OutcomeFailed:
			reason := res.ResultDesc
			if reason == "" {
				reason = "payout failed at provider"
			}
			return s.releaseTx(ctx, tx, w, "failed", res.ResultCode, reason)
		}
		return nil
	})
}

// Poll checks payout status where the provider supports it; otherwise it is
// a watchdog that flags overdue results for review.
func (s *Service) Poll(ctx context.Context, id uuid.UUID) error {
	w, err := Get(ctx, s.Pool, id)
	if err != nil {
		return outbox.Permanent(err)
	}
	if IsTerminal(w.Status) || w.Status != "processing" {
		return nil
	}
	prov, ok := s.Providers.Get(w.Provider)
	if !ok {
		return outbox.Permanent(fmt.Errorf("provider %s not configured", w.Provider))
	}
	age := time.Since(w.CreatedAt)
	if w.DispatchStartedAt != nil {
		age = time.Since(*w.DispatchStartedAt)
	}
	if !prov.Capabilities().PayoutStatusQuery {
		if age >= ResultWatchdog && !w.NeedsReview {
			_, err := s.Pool.Exec(ctx, `UPDATE withdrawals SET needs_review = true, updated_at = now() WHERE id = $1 AND status = 'processing'`, id)
			s.Log.Warn("withdrawal result overdue; flagged for review", "withdrawal_id", id)
			return err
		}
		if age < ResultWatchdog {
			return outbox.RetryAfter(ResultWatchdog-age, "awaiting result callback")
		}
		return nil
	}
	res, qerr := prov.QueryPayout(ctx, payments.StatusQuery{RequestRef: deref(w.ProviderRequestRef), CheckoutRef: deref(w.ProviderConvRef), MSISDN: w.MSISDN, Amount: w.Amount})
	deposits.ProviderCallMetric(w.Provider, "payout.query", qerr)
	if qerr == nil && (res.Outcome == payments.OutcomeSucceeded || res.Outcome == payments.OutcomeFailed) {
		return s.ApplyOutcome(ctx, id, res)
	}
	if age > PollHorizon {
		_, err := s.Pool.Exec(ctx, `UPDATE withdrawals SET needs_review = true, outcome_uncertain = true, updated_at = now() WHERE id = $1`, id)
		return err
	}
	interval := 10 * time.Second
	switch {
	case age > time.Hour:
		interval = 10 * time.Minute
	case age > 5*time.Minute:
		interval = time.Minute
	}
	return outbox.RetryAfter(interval, "payout still pending")
}

// MarkTimeout records a provider queue-timeout notification. The outcome is
// unknown, so funds stay held.
func (s *Service) MarkTimeout(ctx context.Context, id uuid.UUID, desc string) error {
	return db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		w, err := lock(ctx, tx, id)
		if err != nil || IsTerminal(w.Status) {
			return err
		}
		return s.markUncertainTx(ctx, tx, id, "provider queue timeout: "+desc)
	})
}

// Resolve is the audited admin path for an uncertain payout once evidence
// (e.g. provider portal statement) establishes the real outcome.
func (s *Service) Resolve(ctx context.Context, id uuid.UUID, outcome, receipt, evidence string) (*Withdrawal, error) {
	evidence = strings.TrimSpace(evidence)
	if len(evidence) < 10 {
		return nil, apperr.Field("evidence", "describe the provider evidence (at least 10 characters)")
	}
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		w, err := lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if w.Status != "processing" {
			return apperr.New(apperr.CodeInvalidState, "only processing withdrawals can be resolved")
		}
		switch outcome {
		case "succeeded":
			if strings.TrimSpace(receipt) == "" {
				return apperr.Field("provider_receipt", "required when resolving as succeeded")
			}
			err = s.settleTx(ctx, tx, w, strings.TrimSpace(receipt), "MANUAL", "resolved by admin")
		case "failed":
			err = s.releaseTx(ctx, tx, w, "failed", "MANUAL", "payout confirmed failed by operations")
		default:
			return apperr.Field("outcome", "must be succeeded or failed")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE withdrawals SET outcome_uncertain = false, needs_review = false WHERE id = $1`, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "withdrawal.resolved", TargetType: "withdrawal", TargetID: id.String(),
			Details: map[string]any{"outcome": outcome, "receipt": receipt, "evidence": evidence}})
	})
	if err != nil {
		return nil, err
	}
	return Get(ctx, s.Pool, id)
}

// Reverse books money returned by the provider after a successful payout
// (approved maker-checker workflow only). The fee is not refunded here.
func Reverse(ctx context.Context, tx pgx.Tx, id, actor uuid.UUID) (uuid.UUID, error) {
	w, err := lock(ctx, tx, id)
	if err != nil {
		return uuid.Nil, err
	}
	if w.Status != "succeeded" || w.SettleJournalID == nil {
		return uuid.Nil, apperr.New(apperr.CodeInvalidState, "only succeeded withdrawals can be reversed")
	}
	wl, err := wallet.Get(ctx, tx, w.WalletID)
	if err != nil {
		return uuid.Nil, err
	}
	clearing, err := ledger.ProviderClearingAccount(w.Provider, w.Currency)
	if err != nil {
		return uuid.Nil, err
	}
	jid, err := ledger.Post(ctx, tx, ledger.Journal{
		PostingKey: "withdrawal:" + id.String() + ":reversal", Kind: "withdrawal_reversal", ReferenceType: "withdrawal",
		ReferenceID: id, ReversesJournalID: w.SettleJournalID, Description: "Payout reversed by provider", CreatedBy: &actor,
		Currency: w.Currency, Entries: []ledger.Entry{
			{AccountID: clearing, Direction: ledger.Debit, Amount: w.Amount},
			{AccountID: wl.AvailableAccountID, Direction: ledger.Credit, Amount: w.Amount},
		},
	})
	if err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE withdrawals SET status = 'reversed', reversal_journal_id = $2, updated_at = now() WHERE id = $1`, id, jid)
	return jid, err
}

// Sweep re-queues dispatch for pending withdrawals whose job died, and
// ensures processing withdrawals have a poll/watchdog job.
func (s *Service) Sweep(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO outbox_jobs (kind, payload, dedupe_key, max_attempts)
		SELECT $1, jsonb_build_object('withdrawal_id', w.id::text), $1 || ':' || w.id::text, 1000
		  FROM withdrawals w
		 WHERE w.status = 'processing' AND w.needs_review = false AND w.updated_at < now() - interval '2 minutes'
		ON CONFLICT (dedupe_key) DO NOTHING`, JobPoll)
	if err != nil {
		return err
	}
	// A payout whose dispatch job gave up while the request provably never
	// left (not_sent) is definitively failed: release the held funds.
	rows, err := s.Pool.Query(ctx, `
		SELECT w.id FROM withdrawals w
		  JOIN outbox_jobs j ON j.dedupe_key = $1 || ':' || w.id::text AND j.status = 'dead'
		 WHERE w.status = 'pending' AND w.provider_status = 'not_sent'
		 LIMIT 100`, JobDispatch)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
			w, err := lock(ctx, tx, id)
			if err != nil || w.Status != "pending" || w.ProviderStatus != "not_sent" {
				return err
			}
			return s.releaseTx(ctx, tx, w, "rejected", "", "could not reach the payment provider")
		})
		if err != nil {
			s.Log.Error("sweep withdrawal", "withdrawal_id", id, "err", err)
		}
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// RequestRecheck schedules an immediate status poll.
func RequestRecheck(ctx context.Context, q db.Querier, id uuid.UUID) error {
	_, err := q.Exec(ctx, `
		INSERT INTO outbox_jobs (kind, payload, dedupe_key, run_at, max_attempts)
		VALUES ($1, jsonb_build_object('withdrawal_id', $2::text), $3, now(), 1000)
		ON CONFLICT (dedupe_key) DO UPDATE SET run_at = now(), status = 'pending', updated_at = now()
		 WHERE outbox_jobs.status IN ('pending', 'done', 'dead')`,
		JobPoll, id.String(), JobPoll+":"+id.String())
	return err
}

func FlagReview(ctx context.Context, q db.Querier, id uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE withdrawals SET needs_review = true, updated_at = now() WHERE id = $1`, id)
	return err
}
