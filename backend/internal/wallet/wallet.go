// Package wallet manages customer wallets and read models over the ledger.
package wallet

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/ledger"
)

type Wallet struct {
	ID                 uuid.UUID `json:"id"`
	UserID             uuid.UUID `json:"user_id"`
	AccountNumber      string    `json:"account_number"`
	Currency           string    `json:"currency"`
	Status             string    `json:"status"`
	AvailableAccountID uuid.UUID `json:"-"`
	HoldAccountID      uuid.UUID `json:"-"`
	CreatedAt          time.Time `json:"created_at"`
}

const cols = `id, user_id, account_number, currency, status, available_account_id, hold_account_id, created_at`

func scan(row pgx.Row) (Wallet, error) {
	var w Wallet
	err := row.Scan(&w.ID, &w.UserID, &w.AccountNumber, &w.Currency, &w.Status, &w.AvailableAccountID, &w.HoldAccountID, &w.CreatedAt)
	return w, err
}

// Create opens a wallet and its ledger accounts inside tx.
func Create(ctx context.Context, tx pgx.Tx, userID uuid.UUID, currency string) (Wallet, error) {
	avail, hold, err := ledger.CreateWalletAccounts(ctx, tx, currency)
	if err != nil {
		return Wallet{}, err
	}
	for attempt := 0; attempt < 5; attempt++ {
		num, err := newAccountNumber()
		if err != nil {
			return Wallet{}, err
		}
		_, err = tx.Exec(ctx, `SAVEPOINT wallet_number`)
		if err != nil {
			return Wallet{}, err
		}
		w, err := scan(tx.QueryRow(ctx, `
			INSERT INTO wallets (id, user_id, account_number, currency, available_account_id, hold_account_id)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+cols,
			uuid.Must(uuid.NewV7()), userID, num, currency, avail, hold))
		if err == nil {
			return w, nil
		}
		if !db.IsUniqueViolation(err, "wallets_account_number_key") {
			return Wallet{}, err
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT wallet_number`); err != nil {
			return Wallet{}, err
		}
	}
	return Wallet{}, errors.New("could not allocate a unique account number")
}

// newAccountNumber returns 11 random digits plus a Luhn check digit.
func newAccountNumber() (string, error) {
	var sb strings.Builder
	for i := 0; i < 11; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		if i == 0 && n.Int64() == 0 {
			n = big.NewInt(1)
		}
		sb.WriteByte(byte('0' + n.Int64()))
	}
	s := sb.String()
	return s + string(rune('0'+luhnCheckDigit(s))), nil
}

func luhnCheckDigit(s string) int {
	sum := 0
	double := true
	for i := len(s) - 1; i >= 0; i-- {
		d := int(s[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return (10 - sum%10) % 10
}

// ValidAccountNumber checks format and Luhn digit.
func ValidAccountNumber(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return luhnCheckDigit(s[:11]) == int(s[11]-'0')
}

func ListForUser(ctx context.Context, q db.Querier, userID uuid.UUID) ([]Wallet, error) {
	rows, err := q.Query(ctx, `SELECT `+cols+` FROM wallets WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Wallet, error) { return scan(r) })
}

// GetOwned returns a wallet only if userID owns it. Non-owned wallets are
// reported as not found to avoid leaking existence.
func GetOwned(ctx context.Context, q db.Querier, walletID, userID uuid.UUID) (Wallet, error) {
	w, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM wallets WHERE id = $1 AND user_id = $2`, walletID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, apperr.NotFound("wallet")
	}
	return w, err
}

func Get(ctx context.Context, q db.Querier, walletID uuid.UUID) (Wallet, error) {
	w, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM wallets WHERE id = $1`, walletID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, apperr.NotFound("wallet")
	}
	return w, err
}

// PrimaryForUser returns the user's wallet in the given currency.
func PrimaryForUser(ctx context.Context, q db.Querier, userID uuid.UUID, currency string) (Wallet, error) {
	w, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM wallets WHERE user_id = $1 AND currency = $2`, userID, currency))
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, apperr.NotFound("wallet")
	}
	return w, err
}

func GetByAccountNumber(ctx context.Context, q db.Querier, num string) (Wallet, error) {
	w, err := scan(q.QueryRow(ctx, `SELECT `+cols+` FROM wallets WHERE account_number = $1`, num))
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, apperr.NotFound("wallet")
	}
	return w, err
}

// LockForUpdate serialises money-moving operations initiated by a wallet,
// covering limit checks as well as the ledger posting.
func LockForUpdate(ctx context.Context, tx pgx.Tx, walletID uuid.UUID) (Wallet, error) {
	w, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM wallets WHERE id = $1 FOR UPDATE`, walletID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, apperr.NotFound("wallet")
	}
	return w, err
}

type Balance struct {
	Available int64
	Held      int64
}

func (b Balance) Total() int64 { return b.Available + b.Held }

func GetBalance(ctx context.Context, q db.Querier, w Wallet) (Balance, error) {
	var b Balance
	err := q.QueryRow(ctx, `
		SELECT (SELECT balance FROM ledger_accounts WHERE id = $1),
		       (SELECT balance FROM ledger_accounts WHERE id = $2)`,
		w.AvailableAccountID, w.HoldAccountID).Scan(&b.Available, &b.Held)
	return b, err
}

// PendingInbound sums deposits not yet confirmed (not spendable).
func PendingInbound(ctx context.Context, q db.Querier, walletID uuid.UUID) (int64, error) {
	var v int64
	err := q.QueryRow(ctx, `SELECT coalesce(sum(amount), 0) FROM deposits WHERE wallet_id = $1 AND status IN ('created','pending_provider')`, walletID).Scan(&v)
	return v, err
}

// Activity is one row of the customer-facing history.
type Activity struct {
	WalletID           uuid.UUID  `json:"wallet_id"`
	ID                 uuid.UUID  `json:"id"`
	Type               string     `json:"type"`
	Direction          string     `json:"direction"`
	Amount             int64      `json:"-"`
	Fee                int64      `json:"-"`
	Currency           string     `json:"currency"`
	Status             string     `json:"status"`
	Provider           *string    `json:"provider"`
	Counterparty       *string    `json:"counterparty"`
	CounterpartyDetail *string    `json:"counterparty_detail"`
	Reference          *string    `json:"reference"`
	Description        string     `json:"description"`
	FailureReason      *string    `json:"failure_reason"`
	CreatedAt          time.Time  `json:"created_at"`
	CompletedAt        *time.Time `json:"completed_at"`
}

type ActivityFilter struct {
	Types    []string // deposit, withdrawal, transfer_in, transfer_out, adjustment
	Statuses []string
	From     *time.Time
	To       *time.Time
	Query    string
	// Keyset cursor
	BeforeTime *time.Time
	BeforeID   *uuid.UUID
	Limit      int
}

const activityCols = `wallet_id, id, type, direction, amount, fee, currency, status, provider, counterparty,
	counterparty_detail, reference, description, failure_reason, created_at, completed_at`

func scanActivity(row pgx.Row) (Activity, error) {
	var a Activity
	err := row.Scan(&a.WalletID, &a.ID, &a.Type, &a.Direction, &a.Amount, &a.Fee, &a.Currency, &a.Status, &a.Provider,
		&a.Counterparty, &a.CounterpartyDetail, &a.Reference, &a.Description, &a.FailureReason, &a.CreatedAt, &a.CompletedAt)
	return a, err
}

func ListActivity(ctx context.Context, q db.Querier, walletID uuid.UUID, f ActivityFilter) ([]Activity, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 30
	}
	var search *string
	if s := strings.TrimSpace(f.Query); s != "" {
		if len(s) > 64 {
			s = s[:64]
		}
		p := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s) + "%"
		search = &p
	}
	rows, err := q.Query(ctx, `
		SELECT `+activityCols+` FROM wallet_activity
		 WHERE wallet_id = $1
		   AND (cardinality($2::text[]) = 0 OR type = ANY($2))
		   AND (cardinality($3::text[]) = 0 OR status = ANY($3))
		   AND ($4::timestamptz IS NULL OR created_at >= $4)
		   AND ($5::timestamptz IS NULL OR created_at < $5)
		   AND ($6::text IS NULL OR counterparty ILIKE $6 OR counterparty_detail ILIKE $6
		        OR reference ILIKE $6 OR description ILIKE $6 OR id::text ILIKE $6)
		   AND ($7::timestamptz IS NULL OR (created_at, id) < ($7, $8::uuid))
		 ORDER BY created_at DESC, id DESC
		 LIMIT $9`,
		walletID, nonNil(f.Types), nonNil(f.Statuses), f.From, f.To, search, f.BeforeTime, f.BeforeID, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Activity, error) { return scanActivity(r) })
}

// GetActivity returns one activity row for a wallet (ownership enforced by
// the caller passing the user's wallet).
func GetActivity(ctx context.Context, q db.Querier, walletID, id uuid.UUID) (Activity, error) {
	a, err := scanActivity(q.QueryRow(ctx, `SELECT `+activityCols+` FROM wallet_activity WHERE wallet_id = $1 AND id = $2`, walletID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Activity{}, apperr.NotFound("transaction")
	}
	return a, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// BalanceAt returns the available balance at an instant, derived from entries.
func BalanceAt(ctx context.Context, q db.Querier, accountID uuid.UUID, at time.Time) (int64, error) {
	var v int64
	err := q.QueryRow(ctx, `
		SELECT coalesce(sum(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END), 0)
		  FROM ledger_entries e JOIN journal_transactions j ON j.id = e.journal_id
		 WHERE e.account_id = $1 AND j.posted_at < $2`, accountID, at).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("balance at: %w", err)
	}
	return v, nil
}
