// Package ledger implements double-entry posting on PostgreSQL.
//
// Invariants (enforced in the database, re-checked here for early errors):
//   - every journal balances per currency (deferred constraint trigger);
//   - posted journals and entries are immutable (triggers);
//   - account balances are a projection updated only by the entry trigger;
//   - customer wallet accounts cannot go negative (CHECK constraint);
//   - a business event posts at most once (unique posting_key).
//
// Concurrency: Post locks every touched account row with SELECT ... FOR UPDATE
// in ascending UUID order before inserting entries, so concurrent postings over
// overlapping accounts serialise without deadlocking.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/money"
)

type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// System accounts seeded by migration 00002.
var (
	MpesaClearingKES  = uuid.MustParse("00000000-0000-0000-0000-000000000101")
	AirtelClearingKES = uuid.MustParse("00000000-0000-0000-0000-000000000102")
	FeeRevenueKES     = uuid.MustParse("00000000-0000-0000-0000-000000000201")
	AdjustmentsKES    = uuid.MustParse("00000000-0000-0000-0000-000000000301")
)

// ProviderClearingAccount returns the asset account representing funds held
// at an external provider.
func ProviderClearingAccount(provider, currency string) (uuid.UUID, error) {
	if currency != money.KES.Code {
		return uuid.Nil, fmt.Errorf("no clearing account for %s/%s", provider, currency)
	}
	switch provider {
	case "mpesa":
		return MpesaClearingKES, nil
	case "airtel":
		return AirtelClearingKES, nil
	}
	return uuid.Nil, fmt.Errorf("no clearing account for provider %q", provider)
}

func FeeRevenueAccount(currency string) (uuid.UUID, error) {
	if currency != money.KES.Code {
		return uuid.Nil, fmt.Errorf("no fee revenue account for %s", currency)
	}
	return FeeRevenueKES, nil
}

func AdjustmentsAccount(currency string) (uuid.UUID, error) {
	if currency != money.KES.Code {
		return uuid.Nil, fmt.Errorf("no adjustments account for %s", currency)
	}
	return AdjustmentsKES, nil
}

type Entry struct {
	AccountID uuid.UUID
	Direction Direction
	Amount    int64
}

type Journal struct {
	PostingKey        string
	Kind              string
	ReferenceType     string
	ReferenceID       uuid.UUID
	ReversesJournalID *uuid.UUID
	Description       string
	CreatedBy         *uuid.UUID
	Currency          string
	Entries           []Entry
}

var (
	ErrAlreadyPosted     = errors.New("journal already posted for this posting key")
	ErrUnbalanced        = errors.New("journal is not balanced")
	ErrInvalidJournal    = errors.New("invalid journal")
	ErrInsufficientFunds = errors.New("insufficient funds")
)

// Validate checks structural invariants without touching the database.
func Validate(j Journal) error {
	if j.PostingKey == "" || j.Kind == "" || j.ReferenceType == "" || j.ReferenceID == uuid.Nil || j.Currency == "" {
		return fmt.Errorf("%w: missing header fields", ErrInvalidJournal)
	}
	if len(j.Entries) < 2 {
		return fmt.Errorf("%w: at least two entries required", ErrInvalidJournal)
	}
	var debits, credits int64
	var err error
	for _, e := range j.Entries {
		if e.Amount <= 0 || e.Amount > money.MaxMinor {
			return fmt.Errorf("%w: entry amount must be positive and bounded", ErrInvalidJournal)
		}
		if e.AccountID == uuid.Nil {
			return fmt.Errorf("%w: entry without account", ErrInvalidJournal)
		}
		switch e.Direction {
		case Debit:
			debits, err = money.Add(debits, e.Amount)
		case Credit:
			credits, err = money.Add(credits, e.Amount)
		default:
			return fmt.Errorf("%w: bad direction %q", ErrInvalidJournal, e.Direction)
		}
		if err != nil {
			return err
		}
	}
	if debits != credits {
		return fmt.Errorf("%w: debits %d != credits %d", ErrUnbalanced, debits, credits)
	}
	return nil
}

// LockAccounts takes row locks on the given accounts in ascending order.
func LockAccounts(ctx context.Context, tx pgx.Tx, ids ...uuid.UUID) error {
	uniq := make(map[uuid.UUID]struct{}, len(ids))
	sorted := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, ok := uniq[id]; !ok {
			uniq[id] = struct{}{}
			sorted = append(sorted, id)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return lessUUID(sorted[i], sorted[j]) })
	// LockRows executes above Sort, so rows are locked in ORDER BY order.
	rows, err := tx.Query(ctx, `SELECT id FROM ledger_accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, sorted)
	if err != nil {
		return err
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n != len(sorted) {
		return fmt.Errorf("%w: unknown ledger account", ErrInvalidJournal)
	}
	return nil
}

// lessUUID orders UUIDs bytewise, matching PostgreSQL's uuid ordering.
func lessUUID(a, b uuid.UUID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Post writes a balanced journal inside the caller's transaction. If the
// posting key was already used, it returns the existing journal ID together
// with ErrAlreadyPosted and writes nothing.
func Post(ctx context.Context, tx pgx.Tx, j Journal) (uuid.UUID, error) {
	if err := Validate(j); err != nil {
		return uuid.Nil, err
	}
	var existing uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM journal_transactions WHERE posting_key = $1`, j.PostingKey).Scan(&existing)
	if err == nil {
		return existing, ErrAlreadyPosted
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}

	ids := make([]uuid.UUID, len(j.Entries))
	for i, e := range j.Entries {
		ids[i] = e.AccountID
	}
	if err := LockAccounts(ctx, tx, ids...); err != nil {
		return uuid.Nil, err
	}

	id := uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_transactions (id, posting_key, kind, reference_type, reference_id, reverses_journal_id, description, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, j.PostingKey, j.Kind, j.ReferenceType, j.ReferenceID, j.ReversesJournalID, j.Description, j.CreatedBy)
	if err != nil {
		if db.IsUniqueViolation(err, "journal_transactions_posting_key_key") {
			return uuid.Nil, ErrAlreadyPosted
		}
		return uuid.Nil, err
	}
	batch := &pgx.Batch{}
	for _, e := range j.Entries {
		batch.Queue(`INSERT INTO ledger_entries (journal_id, account_id, currency, direction, amount) VALUES ($1, $2, $3, $4, $5)`,
			id, e.AccountID, j.Currency, string(e.Direction), e.Amount)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		if db.IsCheckViolation(err, "ledger_accounts_non_negative") {
			return uuid.Nil, ErrInsufficientFunds
		}
		return uuid.Nil, err
	}
	return id, nil
}

// Reverse posts a compensating journal that mirrors the original's entries
// with directions swapped.
func Reverse(ctx context.Context, tx pgx.Tx, originalID uuid.UUID, kind, postingKey, description string, createdBy *uuid.UUID) (uuid.UUID, error) {
	var refType, currency string
	var refID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT j.reference_type, j.reference_id, min(e.currency)
		  FROM journal_transactions j JOIN ledger_entries e ON e.journal_id = j.id
		 WHERE j.id = $1 GROUP BY j.id`, originalID).Scan(&refType, &refID, &currency)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load journal to reverse: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT account_id, direction, amount FROM ledger_entries WHERE journal_id = $1 ORDER BY id`, originalID)
	if err != nil {
		return uuid.Nil, err
	}
	var entries []Entry
	for rows.Next() {
		var e Entry
		var dir string
		if err := rows.Scan(&e.AccountID, &dir, &e.Amount); err != nil {
			rows.Close()
			return uuid.Nil, err
		}
		if Direction(dir) == Debit {
			e.Direction = Credit
		} else {
			e.Direction = Debit
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, err
	}
	return Post(ctx, tx, Journal{
		PostingKey: postingKey, Kind: kind, ReferenceType: refType, ReferenceID: refID,
		ReversesJournalID: &originalID, Description: description, CreatedBy: createdBy,
		Currency: currency, Entries: entries,
	})
}

// CreateWalletAccounts creates the available and hold accounts for a wallet.
func CreateWalletAccounts(ctx context.Context, tx pgx.Tx, currency string) (available, hold uuid.UUID, err error) {
	available, hold = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_accounts (id, kind, currency, normal_side, allow_negative) VALUES
		($1, 'wallet_available', $3, 'credit', false),
		($2, 'wallet_hold', $3, 'credit', false)`, available, hold, currency)
	return available, hold, err
}

// Balance returns the projected balance of an account.
func Balance(ctx context.Context, q db.Querier, accountID uuid.UUID) (int64, error) {
	var b int64
	err := q.QueryRow(ctx, `SELECT balance FROM ledger_accounts WHERE id = $1`, accountID).Scan(&b)
	return b, err
}

// DerivedBalance recomputes an account balance from its entries.
func DerivedBalance(ctx context.Context, q db.Querier, accountID uuid.UUID) (int64, error) {
	var b int64
	err := q.QueryRow(ctx, `
		SELECT coalesce(sum(CASE WHEN e.direction = a.normal_side THEN e.amount ELSE -e.amount END), 0)
		  FROM ledger_accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
		 WHERE a.id = $1 GROUP BY a.id`, accountID).Scan(&b)
	return b, err
}

// DriftRow describes an account whose projection disagrees with its entries.
type DriftRow struct {
	AccountID uuid.UUID
	Code      *string
	Kind      string
	Currency  string
	Projected int64
	Derived   int64
}

// Drift lists accounts whose balance projection disagrees with the entries.
func Drift(ctx context.Context, q db.Querier) ([]DriftRow, error) {
	rows, err := q.Query(ctx, `SELECT account_id, code, kind, currency, projected_balance, derived_balance FROM ledger_projection_drift`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DriftRow, error) {
		var d DriftRow
		err := r.Scan(&d.AccountID, &d.Code, &d.Kind, &d.Currency, &d.Projected, &d.Derived)
		return d, err
	})
}

// TrialBalance returns sum(debits) - sum(credits) per currency over all
// entries; every value must be zero.
func TrialBalance(ctx context.Context, q db.Querier) (map[string]int64, error) {
	rows, err := q.Query(ctx, `
		SELECT currency, coalesce(sum(CASE WHEN direction = 'debit' THEN amount ELSE -amount END), 0)
		  FROM ledger_entries GROUP BY currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var c string
		var v int64
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		out[c] = v
	}
	return out, rows.Err()
}
