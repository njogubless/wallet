// Package transfers implements atomic wallet-to-wallet transfers.
//
// A transfer is created and settled in one database transaction: the sender
// wallet row is locked (serialising limit checks and spending), the balanced
// journal is posted (the database rejects overdrafts), the transfer row is
// written and the idempotency key completed. There is no intermediate state
// in which money has left the sender but not reached the recipient.
package transfers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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
	"github.com/sovereigntrust/wallet/internal/phone"
	"github.com/sovereigntrust/wallet/internal/policy"
	"github.com/sovereigntrust/wallet/internal/wallet"
)

var transfersTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "wallet_transfers_total", Help: "Internal transfers by outcome.",
}, []string{"outcome"})

func init() { prometheus.MustRegister(transfersTotal) }

type Transfer struct {
	ID                uuid.UUID  `json:"id"`
	SenderUserID      uuid.UUID  `json:"-"`
	SenderWalletID    uuid.UUID  `json:"sender_wallet_id"`
	RecipientUserID   uuid.UUID  `json:"-"`
	RecipientWalletID uuid.UUID  `json:"recipient_wallet_id"`
	Amount            int64      `json:"-"`
	Fee               int64      `json:"-"`
	Currency          string     `json:"currency"`
	Status            string     `json:"status"`
	Description       string     `json:"description"`
	FailureReason     *string    `json:"failure_reason"`
	JournalID         *uuid.UUID `json:"-"`
	ReversalJournalID *uuid.UUID `json:"-"`
	CreatedAt         time.Time  `json:"created_at"`
	CompletedAt       *time.Time `json:"completed_at"`
	RecipientName     string     `json:"-"`
	RecipientAccount  string     `json:"-"`
	SenderName        string     `json:"-"`
	SenderAccount     string     `json:"-"`
}

const cols = `t.id, t.sender_user_id, t.sender_wallet_id, t.recipient_user_id, t.recipient_wallet_id, t.amount, t.fee, t.currency,
	t.status, t.description, t.failure_reason, t.journal_id, t.reversal_journal_id, t.created_at, t.completed_at,
	ru.full_name, rw.account_number, su.full_name, sw.account_number`

const from = ` FROM transfers t
	JOIN users ru ON ru.id = t.recipient_user_id JOIN wallets rw ON rw.id = t.recipient_wallet_id
	JOIN users su ON su.id = t.sender_user_id JOIN wallets sw ON sw.id = t.sender_wallet_id`

func scan(row pgx.Row) (*Transfer, error) {
	var t Transfer
	err := row.Scan(&t.ID, &t.SenderUserID, &t.SenderWalletID, &t.RecipientUserID, &t.RecipientWalletID, &t.Amount, &t.Fee,
		&t.Currency, &t.Status, &t.Description, &t.FailureReason, &t.JournalID, &t.ReversalJournalID, &t.CreatedAt, &t.CompletedAt,
		&t.RecipientName, &t.RecipientAccount, &t.SenderName, &t.SenderAccount)
	return &t, err
}

func Get(ctx context.Context, q db.Querier, id uuid.UUID) (*Transfer, error) {
	t, err := scan(q.QueryRow(ctx, `SELECT `+cols+from+` WHERE t.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("transfer")
	}
	return t, err
}

// GetForUser returns a transfer visible to the user (as sender or recipient).
func GetForUser(ctx context.Context, q db.Querier, id, userID uuid.UUID) (*Transfer, error) {
	t, err := Get(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if t.SenderUserID != userID && t.RecipientUserID != userID {
		return nil, apperr.NotFound("transfer")
	}
	return t, nil
}

type Service struct {
	Pool                     *pgxpool.Pool
	Auth                     *auth.Service
	Idem                     *idempotency.Store
	RequirePhoneVerification bool
}

type CreateInput struct {
	UserID uuid.UUID
	// Exactly one of RecipientPhone / RecipientAccount.
	RecipientPhone   string
	RecipientAccount string
	Amount           string
	Currency         string
	Description      string
	PIN              string
	IdempotencyKey   string
}

type Quote struct {
	Amount   int64
	Fee      int64
	Total    int64
	Currency money.Currency
}

// Recipient is the minimal, privacy-preserving view of a transfer target.
type Recipient struct {
	WalletID      uuid.UUID `json:"-"`
	UserID        uuid.UUID `json:"-"`
	DisplayName   string    `json:"display_name"`
	PhoneMasked   string    `json:"phone_masked"`
	AccountNumber string    `json:"account_number"`
}

// MaskName turns "Amina Otieno" into "Amina O.".
func MaskName(full string) string {
	parts := strings.Fields(full)
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	if len(parts) > 1 {
		r, _ := utf8.DecodeRuneInString(parts[len(parts)-1])
		out += " " + string(r) + "."
	}
	return out
}

// LookupRecipient resolves a recipient by phone or account number.
func LookupRecipient(ctx context.Context, q db.Querier, phoneIn, account, currency string) (*Recipient, error) {
	var where string
	var arg string
	switch {
	case phoneIn != "" && account == "":
		ph, err := phone.NormalizeKE(phoneIn)
		if err != nil {
			return nil, apperr.Field("phone", "must be a valid Kenyan mobile number")
		}
		where, arg = "u.phone = $1", ph
	case account != "" && phoneIn == "":
		if !wallet.ValidAccountNumber(account) {
			return nil, apperr.Field("account_number", "invalid account number")
		}
		where, arg = "w.account_number = $1", account
	default:
		return nil, apperr.Validation("provide exactly one of phone or account_number", nil)
	}
	var r Recipient
	var full, ph, status, wstatus string
	err := q.QueryRow(ctx, `
		SELECT w.id, u.id, u.full_name, u.phone, w.account_number, u.status, w.status
		  FROM wallets w JOIN users u ON u.id = w.user_id
		 WHERE `+where+` AND w.currency = $2`, arg, currency).Scan(&r.WalletID, &r.UserID, &full, &ph, &r.AccountNumber, &status, &wstatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("recipient")
	}
	if err != nil {
		return nil, err
	}
	if wstatus != "active" || (status != "active" && status != "restricted") {
		return nil, apperr.NotFound("recipient")
	}
	r.DisplayName = MaskName(full)
	r.PhoneMasked = phone.Mask(ph)
	return &r, nil
}

// Preview computes the fee for a prospective transfer.
func (s *Service) Preview(ctx context.Context, amountStr, currency string) (Quote, error) {
	cur, err := money.LookupCurrency(currency)
	if err != nil {
		return Quote{}, apperr.Field("currency", "unsupported currency")
	}
	amount, err := money.ParsePositive(amountStr, cur)
	if err != nil {
		return Quote{}, apperr.Field("amount", err.Error())
	}
	fee, err := policy.Fee(ctx, s.Pool, "transfer", "*", cur.Code, amount)
	if err != nil {
		return Quote{}, err
	}
	total, err := money.Add(amount, fee)
	if err != nil {
		return Quote{}, apperr.Field("amount", "amount too large")
	}
	return Quote{Amount: amount, Fee: fee, Total: total, Currency: cur}, nil
}

// Create executes a transfer. replay reports whether this is the stored
// result of an earlier identical request.
func (s *Service) Create(ctx context.Context, in CreateInput) (t *Transfer, replay bool, err error) {
	if in.Currency == "" {
		in.Currency = money.KES.Code
	}
	in.Description = strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(in.Description) > 140 {
		return nil, false, apperr.Field("description", "must be at most 140 characters")
	}
	quote, err := s.Preview(ctx, in.Amount, in.Currency)
	if err != nil {
		return nil, false, err
	}
	key := &idempotency.Key{
		Principal: in.UserID, Operation: "transfer.create", Key: in.IdempotencyKey,
		Hash: idempotency.Fingerprint([]any{in.RecipientPhone, in.RecipientAccount, quote.Amount, quote.Currency.Code, in.Description}),
	}
	prior, err := s.Idem.Acquire(ctx, key)
	if err != nil {
		return nil, false, err
	}
	if prior != nil {
		t, err := Get(ctx, s.Pool, prior.ResourceID)
		return t, true, err
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
	recipient, err := LookupRecipient(ctx, s.Pool, in.RecipientPhone, in.RecipientAccount, quote.Currency.Code)
	if err != nil {
		return nil, false, err
	}
	senderWallet, err := wallet.PrimaryForUser(ctx, s.Pool, in.UserID, quote.Currency.Code)
	if err != nil {
		return nil, false, err
	}
	if recipient.WalletID == senderWallet.ID {
		return nil, false, apperr.Validation("you cannot transfer to your own wallet", nil)
	}

	id := uuid.Must(uuid.NewV7())
	err = db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		sw, err := wallet.LockForUpdate(ctx, tx, senderWallet.ID)
		if err != nil {
			return err
		}
		if sw.Status != "active" {
			return apperr.New(apperr.CodeAccountRestricted, "your wallet is "+sw.Status)
		}
		sender, err := auth.GetUser(ctx, tx, in.UserID)
		if err != nil {
			return err
		}
		if err := sender.CanSend(s.RequirePhoneVerification); err != nil {
			return err
		}
		recipientUser, err := auth.GetUser(ctx, tx, recipient.UserID)
		if err != nil {
			return err
		}
		if err := recipientUser.CanReceive(); err != nil {
			return apperr.NotFound("recipient")
		}
		if err := policy.CheckLimits(ctx, tx, in.UserID, sender.KYCTier, "transfer", quote.Currency.Code, quote.Amount); err != nil {
			return err
		}
		rw, err := wallet.Get(ctx, tx, recipient.WalletID)
		if err != nil {
			return err
		}
		entries := []ledger.Entry{
			{AccountID: sw.AvailableAccountID, Direction: ledger.Debit, Amount: quote.Total},
			{AccountID: rw.AvailableAccountID, Direction: ledger.Credit, Amount: quote.Amount},
		}
		if quote.Fee > 0 {
			feeAcct, err := ledger.FeeRevenueAccount(quote.Currency.Code)
			if err != nil {
				return err
			}
			entries = append(entries, ledger.Entry{AccountID: feeAcct, Direction: ledger.Credit, Amount: quote.Fee})
		}
		jid, err := ledger.Post(ctx, tx, ledger.Journal{
			PostingKey: "transfer:" + id.String(), Kind: "transfer", ReferenceType: "transfer", ReferenceID: id,
			Description: "P2P transfer", CreatedBy: &in.UserID, Currency: quote.Currency.Code, Entries: entries,
		})
		if errors.Is(err, ledger.ErrInsufficientFunds) {
			return apperr.ErrInsufficientFunds
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO transfers (id, sender_user_id, sender_wallet_id, recipient_user_id, recipient_wallet_id,
			                       amount, fee, currency, status, description, journal_id, completed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'succeeded', $9, $10, now())`,
			id, in.UserID, sw.ID, recipient.UserID, rw.ID, quote.Amount, quote.Fee, quote.Currency.Code, in.Description, jid)
		if err != nil {
			return err
		}
		amt := quote.Currency.Code + " " + money.Format(quote.Amount, quote.Currency)
		if err := notify.Create(ctx, tx, in.UserID, "transaction", "Transfer sent",
			fmt.Sprintf("You sent %s to %s.", amt, MaskName(recipientUser.FullName)),
			map[string]any{"transfer_id": id}, "transfer:"+id.String()+":sender"); err != nil {
			return err
		}
		if err := notify.Create(ctx, tx, recipient.UserID, "transaction", "Money received",
			fmt.Sprintf("You received %s from %s.", amt, MaskName(sender.FullName)),
			map[string]any{"transfer_id": id}, "transfer:"+id.String()+":recipient"); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{Action: "transfer.created", TargetType: "transfer", TargetID: id.String(),
			Details: map[string]any{"amount": quote.Amount, "fee": quote.Fee, "currency": quote.Currency.Code}}); err != nil {
			return err
		}
		return s.Idem.CompleteTx(ctx, tx, key, "transfer", id)
	})
	if err != nil {
		outcome := "error"
		if e, ok := apperr.As(err); ok {
			outcome = string(e.Code)
		}
		transfersTotal.WithLabelValues(outcome).Inc()
		return nil, false, err
	}
	committed = true
	transfersTotal.WithLabelValues("succeeded").Inc()
	t, err = Get(ctx, s.Pool, id)
	return t, false, err
}

// Reverse posts a compensating journal for a succeeded transfer. It is only
// called from the approved maker-checker workflow. The recipient must still
// hold the funds; the database rejects the reversal otherwise.
func Reverse(ctx context.Context, tx pgx.Tx, transferID uuid.UUID, actor uuid.UUID, refundFee bool) (uuid.UUID, error) {
	var status, currency string
	var journalID *uuid.UUID
	var fee int64
	var senderWallet, recipientWallet uuid.UUID
	err := tx.QueryRow(ctx, `SELECT status, currency, journal_id, fee, sender_wallet_id, recipient_wallet_id FROM transfers WHERE id = $1 FOR UPDATE`,
		transferID).Scan(&status, &currency, &journalID, &fee, &senderWallet, &recipientWallet)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, apperr.NotFound("transfer")
	}
	if err != nil {
		return uuid.Nil, err
	}
	if status != "succeeded" || journalID == nil {
		return uuid.Nil, apperr.New(apperr.CodeInvalidState, "only succeeded transfers can be reversed")
	}
	sw, err := wallet.Get(ctx, tx, senderWallet)
	if err != nil {
		return uuid.Nil, err
	}
	rw, err := wallet.Get(ctx, tx, recipientWallet)
	if err != nil {
		return uuid.Nil, err
	}
	var amount int64
	if err := tx.QueryRow(ctx, `SELECT amount FROM transfers WHERE id = $1`, transferID).Scan(&amount); err != nil {
		return uuid.Nil, err
	}
	refund := amount
	entries := []ledger.Entry{{AccountID: rw.AvailableAccountID, Direction: ledger.Debit, Amount: amount}}
	if refundFee && fee > 0 {
		feeAcct, err := ledger.FeeRevenueAccount(currency)
		if err != nil {
			return uuid.Nil, err
		}
		entries = append(entries, ledger.Entry{AccountID: feeAcct, Direction: ledger.Debit, Amount: fee})
		refund += fee
	}
	entries = append(entries, ledger.Entry{AccountID: sw.AvailableAccountID, Direction: ledger.Credit, Amount: refund})
	jid, err := ledger.Post(ctx, tx, ledger.Journal{
		PostingKey: "transfer:" + transferID.String() + ":reversal", Kind: "transfer_reversal",
		ReferenceType: "transfer", ReferenceID: transferID, ReversesJournalID: journalID,
		Description: "Transfer reversal", CreatedBy: &actor, Currency: currency, Entries: entries,
	})
	if errors.Is(err, ledger.ErrInsufficientFunds) {
		return uuid.Nil, apperr.New(apperr.CodeInsufficientFunds, "recipient no longer holds the funds; reversal cannot be posted")
	}
	if err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE transfers SET status = 'reversed', reversal_journal_id = $2 WHERE id = $1`, transferID, jid)
	return jid, err
}
