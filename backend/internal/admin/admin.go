// Package admin implements privileged operations for support and admin staff.
//
// There is deliberately no operation that writes a balance. Money can only be
// moved by staff through financial requests: reason-coded, maker-checker
// (approver must differ from requester, enforced by a database constraint),
// audited, and posted as balanced ledger journals.
package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/audit"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/ledger"
	"github.com/sovereigntrust/wallet/internal/money"
	"github.com/sovereigntrust/wallet/internal/notify"
	"github.com/sovereigntrust/wallet/internal/policy"
	"github.com/sovereigntrust/wallet/internal/transfers"
	"github.com/sovereigntrust/wallet/internal/wallet"
	"github.com/sovereigntrust/wallet/internal/withdrawals"
)

type Service struct {
	Pool *pgxpool.Pool
}

type UserSummary struct {
	ID              uuid.UUID  `json:"id"`
	Phone           string     `json:"phone"`
	Email           *string    `json:"email"`
	FullName        string     `json:"full_name"`
	Role            string     `json:"role"`
	Status          string     `json:"status"`
	StatusReason    *string    `json:"status_reason"`
	KYCTier         int        `json:"kyc_tier"`
	PhoneVerifiedAt *time.Time `json:"phone_verified_at"`
	AccountNumber   *string    `json:"account_number"`
	CreatedAt       time.Time  `json:"created_at"`
}

const userSummaryCols = `u.id, u.phone, u.email, u.full_name, u.role, u.status, u.status_reason, u.kyc_tier, u.phone_verified_at, w.account_number, u.created_at`

func scanUser(row pgx.Row) (UserSummary, error) {
	var u UserSummary
	var email *string
	err := row.Scan(&u.ID, &u.Phone, &email, &u.FullName, &u.Role, &u.Status, &u.StatusReason, &u.KYCTier, &u.PhoneVerifiedAt, &u.AccountNumber, &u.CreatedAt)
	u.Email = email
	return u, err
}

func (s *Service) SearchUsers(ctx context.Context, q, status string, limit int) ([]UserSummary, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	q = strings.TrimSpace(q)
	var pattern *string
	if q != "" {
		p := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
		pattern = &p
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+userSummaryCols+` FROM users u LEFT JOIN wallets w ON w.user_id = u.id AND w.currency = 'KES'
		 WHERE ($1::text IS NULL OR u.phone ILIKE $1 OR u.full_name ILIKE $1 OR u.email::text ILIKE $1
		        OR w.account_number ILIKE $1 OR u.id::text = $2)
		   AND ($3 = '' OR u.status = $3)
		 ORDER BY u.created_at DESC LIMIT $4`, pattern, q, status, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (UserSummary, error) { return scanUser(r) })
}

type WalletDetail struct {
	wallet.Wallet
	Available string `json:"available"`
	Held      string `json:"held"`
	Derived   string `json:"derived_available"`
}

type UserDetail struct {
	User    UserSummary    `json:"user"`
	Wallets []WalletDetail `json:"wallets"`
}

func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*UserDetail, error) {
	u, err := scanUser(s.Pool.QueryRow(ctx, `SELECT `+userSummaryCols+` FROM users u LEFT JOIN wallets w ON w.user_id = u.id AND w.currency = 'KES' WHERE u.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("user")
	}
	if err != nil {
		return nil, err
	}
	ws, err := wallet.ListForUser(ctx, s.Pool, id)
	if err != nil {
		return nil, err
	}
	out := &UserDetail{User: u}
	for _, w := range ws {
		b, err := wallet.GetBalance(ctx, s.Pool, w)
		if err != nil {
			return nil, err
		}
		derived, err := ledger.DerivedBalance(ctx, s.Pool, w.AvailableAccountID)
		if err != nil {
			return nil, err
		}
		cur, _ := money.LookupCurrency(w.Currency)
		out.Wallets = append(out.Wallets, WalletDetail{Wallet: w, Available: money.Format(b.Available, cur),
			Held: money.Format(b.Held, cur), Derived: money.Format(derived, cur)})
	}
	return out, nil
}

// SetUserStatus restricts, suspends, closes or reactivates an account.
func (s *Service) SetUserStatus(ctx context.Context, actor, id uuid.UUID, status, reason string) error {
	switch status {
	case "active", "restricted", "suspended", "closed":
	default:
		return apperr.Field("status", "must be active, restricted, suspended or closed")
	}
	reason = strings.TrimSpace(reason)
	if len(reason) < 5 {
		return apperr.Field("reason", "a reason is required")
	}
	if actor == id {
		return apperr.New(apperr.CodeForbidden, "you cannot change your own account status")
	}
	return db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var prev string
		if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&prev); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.NotFound("user")
			}
			return err
		}
		if prev == "closed" {
			return apperr.New(apperr.CodeInvalidState, "closed accounts cannot be changed")
		}
		if status == "closed" {
			var held int64
			if err := tx.QueryRow(ctx, `
				SELECT coalesce(sum(a.balance), 0) FROM wallets w
				  JOIN ledger_accounts a ON a.id IN (w.available_account_id, w.hold_account_id)
				 WHERE w.user_id = $1`, id).Scan(&held); err != nil {
				return err
			}
			if held != 0 {
				return apperr.New(apperr.CodeInvalidState, "account still holds funds; settle the balance before closing")
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET status = $2, status_reason = $3, updated_at = now() WHERE id = $1`, id, status, reason); err != nil {
			return err
		}
		if status == "suspended" || status == "closed" {
			if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id); err != nil {
				return err
			}
		}
		if err := notify.Create(ctx, tx, id, "account", "Account status updated",
			"Your account status is now "+status+". Contact support if you have questions.", nil, ""); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "admin.user_status_changed", TargetType: "user", TargetID: id.String(),
			Details: map[string]any{"from": prev, "to": status, "reason": reason}})
	})
}

// ---- Financial requests (maker-checker) ----

type FinancialRequest struct {
	ID            uuid.UUID  `json:"id"`
	Kind          string     `json:"kind"`
	WalletID      *uuid.UUID `json:"wallet_id"`
	TargetType    *string    `json:"target_type"`
	TargetID      *uuid.UUID `json:"target_id"`
	Amount        *int64     `json:"-"`
	AmountStr     *string    `json:"amount"`
	Currency      *string    `json:"currency"`
	ReasonCode    string     `json:"reason_code"`
	Memo          string     `json:"memo"`
	Status        string     `json:"status"`
	RequestedBy   uuid.UUID  `json:"requested_by"`
	DecidedBy     *uuid.UUID `json:"decided_by"`
	DecidedAt     *time.Time `json:"decided_at"`
	DecisionNote  *string    `json:"decision_note"`
	JournalID     *uuid.UUID `json:"journal_id"`
	FailureReason *string    `json:"failure_reason"`
	CreatedAt     time.Time  `json:"created_at"`
}

const frCols = `id, kind, wallet_id, target_type, target_id, amount, currency, reason_code, memo, status, requested_by,
	decided_by, decided_at, decision_note, journal_id, failure_reason, created_at`

func scanFR(row pgx.Row) (*FinancialRequest, error) {
	var f FinancialRequest
	err := row.Scan(&f.ID, &f.Kind, &f.WalletID, &f.TargetType, &f.TargetID, &f.Amount, &f.Currency, &f.ReasonCode, &f.Memo,
		&f.Status, &f.RequestedBy, &f.DecidedBy, &f.DecidedAt, &f.DecisionNote, &f.JournalID, &f.FailureReason, &f.CreatedAt)
	if err == nil && f.Amount != nil && f.Currency != nil {
		if cur, e := money.LookupCurrency(*f.Currency); e == nil {
			s := money.Format(*f.Amount, cur)
			f.AmountStr = &s
		}
	}
	return &f, err
}

type CreateRequestInput struct {
	Kind       string
	WalletID   *uuid.UUID
	TargetID   *uuid.UUID
	Amount     string
	Currency   string
	ReasonCode string
	Memo       string
}

func (s *Service) CreateFinancialRequest(ctx context.Context, actor uuid.UUID, in CreateRequestInput) (*FinancialRequest, error) {
	id := uuid.Must(uuid.NewV7())
	var amount *int64
	var currency *string
	var targetType *string
	in.Memo = strings.TrimSpace(in.Memo)
	switch in.Kind {
	case "adjustment_credit", "adjustment_debit":
		if in.WalletID == nil {
			return nil, apperr.Field("wallet_id", "required for adjustments")
		}
		if in.Currency == "" {
			in.Currency = money.KES.Code
		}
		cur, err := money.LookupCurrency(in.Currency)
		if err != nil {
			return nil, apperr.Field("currency", "unsupported currency")
		}
		a, err := money.ParsePositive(in.Amount, cur)
		if err != nil {
			return nil, apperr.Field("amount", err.Error())
		}
		w, err := wallet.Get(ctx, s.Pool, *in.WalletID)
		if err != nil {
			return nil, err
		}
		if w.Currency != cur.Code {
			return nil, apperr.Field("currency", "does not match wallet currency")
		}
		amount, currency = &a, &cur.Code
	case "transfer_reversal", "deposit_reversal", "withdrawal_reversal":
		if in.TargetID == nil {
			return nil, apperr.Field("target_id", "required for reversals")
		}
		t := strings.TrimSuffix(in.Kind, "_reversal")
		targetType = &t
		in.WalletID = nil
	default:
		return nil, apperr.Field("kind", "unsupported request kind")
	}
	var f *FinancialRequest
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		f, err = scanFR(tx.QueryRow(ctx, `
			INSERT INTO financial_requests (id, kind, wallet_id, target_type, target_id, amount, currency, reason_code, memo, requested_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+frCols,
			id, in.Kind, in.WalletID, targetType, in.TargetID, amount, currency, in.ReasonCode, in.Memo, actor))
		if err != nil {
			switch {
			case db.IsCheckViolation(err, ""):
				return apperr.Validation("invalid request: check reason_code and memo (10-1000 characters)", nil)
			case db.IsUniqueViolation(err, "financial_requests_one_reversal_uidx"):
				return apperr.New(apperr.CodeConflict, "a reversal for this transaction is already pending or posted")
			}
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "admin.financial_request_created", TargetType: "financial_request",
			TargetID: id.String(), Details: map[string]any{"kind": in.Kind, "amount": amount, "reason_code": in.ReasonCode}})
	})
	return f, err
}

func (s *Service) ListFinancialRequests(ctx context.Context, status string, limit int) ([]*FinancialRequest, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+frCols+` FROM financial_requests WHERE ($1 = '' OR status = $1) ORDER BY created_at DESC LIMIT $2`, status, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*FinancialRequest, error) { return scanFR(r) })
}

// Decide approves (and posts) or rejects a pending request. The decider must
// be a different admin than the requester.
func (s *Service) Decide(ctx context.Context, actor, id uuid.UUID, approve bool, note string) (*FinancialRequest, error) {
	note = strings.TrimSpace(note)
	var postErr error
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		f, err := scanFR(tx.QueryRow(ctx, `SELECT `+frCols+` FROM financial_requests WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("financial request")
		}
		if err != nil {
			return err
		}
		if f.Status != "pending_approval" {
			return apperr.New(apperr.CodeInvalidState, "request already decided")
		}
		if f.RequestedBy == actor {
			return apperr.New(apperr.CodeForbidden, "a different administrator must approve this request")
		}
		if !approve {
			if _, err := tx.Exec(ctx, `UPDATE financial_requests SET status = 'rejected', decided_by = $2, decided_at = now(), decision_note = $3 WHERE id = $1`,
				id, actor, note); err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Event{Action: "admin.financial_request_rejected", TargetType: "financial_request", TargetID: id.String(),
				Details: map[string]any{"note": note}})
		}
		// Post inside a savepoint so a business failure (e.g. insufficient
		// funds) is recorded as 'failed' rather than aborting the decision.
		if _, err := tx.Exec(ctx, `SAVEPOINT post_request`); err != nil {
			return err
		}
		jid, perr := s.post(ctx, tx, f, actor)
		if perr != nil {
			if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT post_request`); err != nil {
				return err
			}
			if _, ok := apperr.As(perr); !ok {
				return perr // infrastructure error: abort everything
			}
			postErr = perr
			if _, err := tx.Exec(ctx, `UPDATE financial_requests SET status = 'failed', decided_by = $2, decided_at = now(), decision_note = $3, failure_reason = $4 WHERE id = $1`,
				id, actor, note, perr.Error()); err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Event{Action: "admin.financial_request_failed", TargetType: "financial_request", TargetID: id.String(),
				Details: map[string]any{"error": perr.Error()}})
		}
		if _, err := tx.Exec(ctx, `UPDATE financial_requests SET status = 'posted', decided_by = $2, decided_at = now(), decision_note = $3, journal_id = $4 WHERE id = $1`,
			id, actor, note, jid); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "admin.financial_request_posted", TargetType: "financial_request", TargetID: id.String(),
			Details: map[string]any{"journal_id": jid, "kind": f.Kind}})
	})
	if err != nil {
		return nil, err
	}
	f, err := scanFR(s.Pool.QueryRow(ctx, `SELECT `+frCols+` FROM financial_requests WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	if postErr != nil {
		return f, postErr
	}
	return f, nil
}

func (s *Service) post(ctx context.Context, tx pgx.Tx, f *FinancialRequest, actor uuid.UUID) (uuid.UUID, error) {
	switch f.Kind {
	case "adjustment_credit", "adjustment_debit":
		w, err := wallet.Get(ctx, tx, *f.WalletID)
		if err != nil {
			return uuid.Nil, err
		}
		adj, err := ledger.AdjustmentsAccount(*f.Currency)
		if err != nil {
			return uuid.Nil, err
		}
		entries := []ledger.Entry{
			{AccountID: adj, Direction: ledger.Debit, Amount: *f.Amount},
			{AccountID: w.AvailableAccountID, Direction: ledger.Credit, Amount: *f.Amount},
		}
		if f.Kind == "adjustment_debit" {
			entries[0].Direction, entries[1].Direction = ledger.Credit, ledger.Debit
		}
		jid, err := ledger.Post(ctx, tx, ledger.Journal{
			PostingKey: "financial_request:" + f.ID.String(), Kind: "adjustment", ReferenceType: "financial_request", ReferenceID: f.ID,
			Description: fmt.Sprintf("%s (%s)", f.Kind, f.ReasonCode), CreatedBy: &actor, Currency: *f.Currency, Entries: entries,
		})
		if errors.Is(err, ledger.ErrInsufficientFunds) {
			return uuid.Nil, apperr.New(apperr.CodeInsufficientFunds, "wallet balance is lower than the debit adjustment")
		}
		if err != nil {
			return uuid.Nil, err
		}
		cur, _ := money.LookupCurrency(*f.Currency)
		verb := "credited to"
		if f.Kind == "adjustment_debit" {
			verb = "debited from"
		}
		return jid, notify.Create(ctx, tx, w.UserID, "transaction", "Account adjustment",
			fmt.Sprintf("%s %s was %s your wallet (%s).", *f.Currency, money.Format(*f.Amount, cur), verb, strings.ReplaceAll(f.ReasonCode, "_", " ")),
			map[string]any{"financial_request_id": f.ID}, "fr:"+f.ID.String())
	case "transfer_reversal":
		return transfers.Reverse(ctx, tx, *f.TargetID, actor, true)
	case "deposit_reversal":
		return deposits.Reverse(ctx, tx, *f.TargetID, actor)
	case "withdrawal_reversal":
		return withdrawals.Reverse(ctx, tx, *f.TargetID, actor)
	}
	return uuid.Nil, apperr.New(apperr.CodeValidation, "unsupported request kind")
}

// ---- Settings ----

func (s *Service) AddFeeRule(ctx context.Context, actor uuid.UUID, r policy.FeeRule) (policy.FeeRule, error) {
	if r.Reason = strings.TrimSpace(r.Reason); len(r.Reason) < 5 {
		return policy.FeeRule{}, apperr.Field("reason", "a reason is required")
	}
	if r.Provider == "" {
		r.Provider = "*"
	}
	r.CreatedBy = &actor
	var out policy.FeeRule
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = policy.InsertFeeRule(ctx, tx, r)
		if err != nil {
			if db.IsCheckViolation(err, "") {
				return apperr.Validation("invalid fee rule values", nil)
			}
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "admin.fee_rule_added", TargetType: "fee_rule", TargetID: out.ID.String(),
			Details: map[string]any{"rule": out}})
	})
	return out, err
}

func (s *Service) AddLimitRule(ctx context.Context, actor uuid.UUID, r policy.LimitRule) (policy.LimitRule, error) {
	if r.Reason = strings.TrimSpace(r.Reason); len(r.Reason) < 5 {
		return policy.LimitRule{}, apperr.Field("reason", "a reason is required")
	}
	r.CreatedBy = &actor
	var out policy.LimitRule
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = policy.InsertLimitRule(ctx, tx, r)
		if err != nil {
			if db.IsCheckViolation(err, "") {
				return apperr.Validation("invalid limit rule values (min <= per-transaction max <= daily max)", nil)
			}
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "admin.limit_rule_added", TargetType: "limit_rule", TargetID: out.ID.String(),
			Details: map[string]any{"rule": out}})
	})
	return out, err
}

// ---- Monitoring ----

type PaymentRow struct {
	Kind           string     `json:"kind"`
	ID             uuid.UUID  `json:"id"`
	UserID         uuid.UUID  `json:"user_id"`
	UserName       string     `json:"user_name"`
	Provider       string     `json:"provider"`
	ProviderEnv    string     `json:"provider_environment"`
	Amount         string     `json:"amount"`
	Fee            string     `json:"fee"`
	Currency       string     `json:"currency"`
	Status         string     `json:"status"`
	ProviderStatus string     `json:"provider_status"`
	NeedsReview    bool       `json:"needs_review"`
	Uncertain      bool       `json:"outcome_uncertain"`
	Receipt        *string    `json:"provider_receipt"`
	ResultCode     *string    `json:"provider_result_code"`
	ResultDesc     *string    `json:"provider_result_desc"`
	FailureReason  *string    `json:"failure_reason"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at"`
	AgeSeconds     int64      `json:"age_seconds"`
}

type PaymentFilter struct {
	Kind        string // deposit | withdrawal | "" (both)
	Status      string
	Provider    string
	NeedsReview bool
	Limit       int
}

func (s *Service) ListPayments(ctx context.Context, f PaymentFilter) ([]PaymentRow, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT * FROM (
		  SELECT 'deposit' AS kind, d.id, d.user_id, u.full_name, d.provider, d.provider_env, d.amount, 0::bigint AS fee, d.currency,
		         d.status, d.provider_status, d.needs_review, false AS uncertain, d.provider_receipt, d.provider_result_code,
		         d.provider_result_desc, d.failure_reason, d.created_at, d.completed_at
		    FROM deposits d JOIN users u ON u.id = d.user_id
		  UNION ALL
		  SELECT 'withdrawal', w.id, w.user_id, u.full_name, w.provider, w.provider_env, w.amount, w.fee, w.currency,
		         w.status, w.provider_status, w.needs_review, w.outcome_uncertain, w.provider_receipt, w.provider_result_code,
		         w.provider_result_desc, w.failure_reason, w.created_at, w.completed_at
		    FROM withdrawals w JOIN users u ON u.id = w.user_id
		) p
		 WHERE ($1 = '' OR kind = $1) AND ($2 = '' OR status = $2) AND ($3 = '' OR provider = $3)
		   AND (NOT $4 OR needs_review OR uncertain)
		 ORDER BY created_at DESC LIMIT $5`, f.Kind, f.Status, f.Provider, f.NeedsReview, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PaymentRow
	for rows.Next() {
		var p PaymentRow
		var amount, fee int64
		if err := rows.Scan(&p.Kind, &p.ID, &p.UserID, &p.UserName, &p.Provider, &p.ProviderEnv, &amount, &fee, &p.Currency,
			&p.Status, &p.ProviderStatus, &p.NeedsReview, &p.Uncertain, &p.Receipt, &p.ResultCode, &p.ResultDesc,
			&p.FailureReason, &p.CreatedAt, &p.CompletedAt); err != nil {
			return nil, err
		}
		cur, _ := money.LookupCurrency(p.Currency)
		p.Amount, p.Fee = money.Format(amount, cur), money.Format(fee, cur)
		p.AgeSeconds = int64(time.Since(p.CreatedAt).Seconds())
		out = append(out, p)
	}
	return out, rows.Err()
}

type Dashboard struct {
	Users               int64             `json:"users"`
	ActiveUsers         int64             `json:"active_users"`
	PendingDeposits     int64             `json:"pending_deposits"`
	ProcessingPayouts   int64             `json:"processing_withdrawals"`
	NeedsReview         int64             `json:"needs_review"`
	OpenReconItems      int64             `json:"open_reconciliation_items"`
	PendingApprovals    int64             `json:"pending_financial_requests"`
	DeadJobs            int64             `json:"dead_jobs"`
	Volume24h           map[string]string `json:"volume_24h"`
	CustomerLiabilities string            `json:"customer_liabilities"`
	LedgerConsistent    bool              `json:"ledger_consistent"`
}

func (s *Service) Dashboard(ctx context.Context) (*Dashboard, error) {
	d := &Dashboard{Volume24h: map[string]string{}}
	err := s.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM users),
		       (SELECT count(*) FROM users WHERE status = 'active'),
		       (SELECT count(*) FROM deposits WHERE status IN ('created','pending_provider')),
		       (SELECT count(*) FROM withdrawals WHERE status IN ('pending','processing')),
		       (SELECT count(*) FROM deposits WHERE needs_review) + (SELECT count(*) FROM withdrawals WHERE needs_review OR outcome_uncertain),
		       (SELECT count(*) FROM reconciliation_items WHERE status IN ('open','investigating')),
		       (SELECT count(*) FROM financial_requests WHERE status = 'pending_approval'),
		       (SELECT count(*) FROM outbox_jobs WHERE status = 'dead')`).
		Scan(&d.Users, &d.ActiveUsers, &d.PendingDeposits, &d.ProcessingPayouts, &d.NeedsReview, &d.OpenReconItems, &d.PendingApprovals, &d.DeadJobs)
	if err != nil {
		return nil, err
	}
	var dep, wd, tr, liab int64
	if err := s.Pool.QueryRow(ctx, `
		SELECT (SELECT coalesce(sum(amount),0) FROM deposits WHERE status = 'succeeded' AND completed_at > now() - interval '24 hours'),
		       (SELECT coalesce(sum(amount),0) FROM withdrawals WHERE status = 'succeeded' AND completed_at > now() - interval '24 hours'),
		       (SELECT coalesce(sum(amount),0) FROM transfers WHERE status = 'succeeded' AND created_at > now() - interval '24 hours'),
		       (SELECT coalesce(sum(balance),0) FROM ledger_accounts WHERE kind IN ('wallet_available','wallet_hold') AND currency = 'KES')`).
		Scan(&dep, &wd, &tr, &liab); err != nil {
		return nil, err
	}
	d.Volume24h["deposits"] = money.Format(dep, money.KES)
	d.Volume24h["withdrawals"] = money.Format(wd, money.KES)
	d.Volume24h["transfers"] = money.Format(tr, money.KES)
	d.CustomerLiabilities = money.Format(liab, money.KES)
	drift, err := ledger.Drift(ctx, s.Pool)
	if err != nil {
		return nil, err
	}
	tb, err := ledger.TrialBalance(ctx, s.Pool)
	if err != nil {
		return nil, err
	}
	d.LedgerConsistent = len(drift) == 0
	for _, v := range tb {
		if v != 0 {
			d.LedgerConsistent = false
		}
	}
	return d, nil
}

type ProviderHealth struct {
	Provider       string     `json:"provider"`
	Environment    string     `json:"environment"`
	Capabilities   any        `json:"capabilities"`
	Attempts24h    int64      `json:"attempts_24h"`
	Succeeded24h   int64      `json:"succeeded_24h"`
	Failed24h      int64      `json:"failed_24h"`
	Unknown24h     int64      `json:"unknown_24h"`
	LastSuccessAt  *time.Time `json:"last_success_at"`
	LastCallbackAt *time.Time `json:"last_callback_at"`
	CallbackErrors int64      `json:"callback_errors_24h"`
}

func (s *Service) ProviderStats(ctx context.Context, provider string) (ProviderHealth, error) {
	h := ProviderHealth{Provider: provider}
	err := s.Pool.QueryRow(ctx, `
		WITH t AS (
		  SELECT status, provider_status, completed_at, created_at FROM deposits WHERE provider = $1 AND created_at > now() - interval '24 hours'
		  UNION ALL
		  SELECT status, provider_status, completed_at, created_at FROM withdrawals WHERE provider = $1 AND created_at > now() - interval '24 hours')
		SELECT count(*), count(*) FILTER (WHERE status = 'succeeded'), count(*) FILTER (WHERE status = 'failed'),
		       count(*) FILTER (WHERE provider_status = 'unknown'),
		       (SELECT max(completed_at) FROM (SELECT completed_at FROM deposits WHERE provider = $1 AND status = 'succeeded'
		                                       UNION ALL SELECT completed_at FROM withdrawals WHERE provider = $1 AND status = 'succeeded') x),
		       (SELECT max(received_at) FROM provider_callbacks WHERE provider = $1),
		       (SELECT count(*) FROM provider_callbacks WHERE provider = $1 AND status = 'error' AND received_at > now() - interval '24 hours')
		  FROM t`, provider).Scan(&h.Attempts24h, &h.Succeeded24h, &h.Failed24h, &h.Unknown24h, &h.LastSuccessAt, &h.LastCallbackAt, &h.CallbackErrors)
	return h, err
}
