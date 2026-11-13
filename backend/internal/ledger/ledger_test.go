package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/ledger"
	"github.com/sovereigntrust/wallet/internal/testutil"
)

func TestValidate(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	base := ledger.Journal{PostingKey: "k", Kind: "transfer", ReferenceType: "t", ReferenceID: uuid.New(), Currency: "KES"}

	j := base
	j.Entries = []ledger.Entry{{a, ledger.Debit, 100}, {b, ledger.Credit, 99}}
	if err := ledger.Validate(j); !errors.Is(err, ledger.ErrUnbalanced) {
		t.Errorf("unbalanced journal: got %v", err)
	}
	j.Entries = []ledger.Entry{{a, ledger.Debit, 100}}
	if err := ledger.Validate(j); !errors.Is(err, ledger.ErrInvalidJournal) {
		t.Errorf("single entry: got %v", err)
	}
	j.Entries = []ledger.Entry{{a, ledger.Debit, 0}, {b, ledger.Credit, 0}}
	if err := ledger.Validate(j); !errors.Is(err, ledger.ErrInvalidJournal) {
		t.Errorf("zero amounts: got %v", err)
	}
	j.Entries = []ledger.Entry{{a, ledger.Debit, 100}, {b, ledger.Credit, 60}, {b, ledger.Credit, 40}}
	if err := ledger.Validate(j); err != nil {
		t.Errorf("valid split journal: %v", err)
	}
}

func newWalletAccount(t *testing.T, pool *pgxpool.Pool) (avail, hold uuid.UUID) {
	t.Helper()
	err := db.WithTx(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		avail, hold, err = ledger.CreateWalletAccounts(context.Background(), tx, "KES")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func fund(t *testing.T, pool *pgxpool.Pool, acct uuid.UUID, amount int64) {
	t.Helper()
	err := db.WithTx(context.Background(), pool, func(tx pgx.Tx) error {
		_, err := ledger.Post(context.Background(), tx, ledger.Journal{
			PostingKey: "fund:" + uuid.NewString(), Kind: "deposit", ReferenceType: "test", ReferenceID: uuid.New(),
			Currency: "KES",
			Entries: []ledger.Entry{
				{ledger.MpesaClearingKES, ledger.Debit, amount},
				{acct, ledger.Credit, amount},
			},
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func transferJournal(from, to uuid.UUID, amount int64, key string) ledger.Journal {
	return ledger.Journal{
		PostingKey: key, Kind: "transfer", ReferenceType: "test", ReferenceID: uuid.New(), Currency: "KES",
		Entries: []ledger.Entry{{from, ledger.Debit, amount}, {to, ledger.Credit, amount}},
	}
}

func post(ctx context.Context, pool *pgxpool.Pool, j ledger.Journal) error {
	return db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := ledger.Post(ctx, tx, j)
		return err
	})
}

func assertNoDrift(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	drift, err := ledger.Drift(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 0 {
		t.Fatalf("projection drift detected: %+v", drift)
	}
	tb, err := ledger.TrialBalance(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	for c, v := range tb {
		if v != 0 {
			t.Fatalf("trial balance for %s is %d, want 0", c, v)
		}
	}
}

func TestLedgerIntegration(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()

	t.Run("posting updates projection and is idempotent by key", func(t *testing.T) {
		a, _ := newWalletAccount(t, pool)
		b, _ := newWalletAccount(t, pool)
		fund(t, pool, a, 10_000)
		if err := post(ctx, pool, transferJournal(a, b, 2_500, "idem-1")); err != nil {
			t.Fatal(err)
		}
		err := post(ctx, pool, transferJournal(a, b, 2_500, "idem-1"))
		if !errors.Is(err, ledger.ErrAlreadyPosted) {
			t.Fatalf("second post with same key: got %v, want ErrAlreadyPosted", err)
		}
		if bal, _ := ledger.Balance(ctx, pool, a); bal != 7_500 {
			t.Errorf("sender balance = %d, want 7500", bal)
		}
		if bal, _ := ledger.Balance(ctx, pool, b); bal != 2_500 {
			t.Errorf("recipient balance = %d, want 2500", bal)
		}
		assertNoDrift(t, pool)
	})

	t.Run("overdraft rejected by database constraint", func(t *testing.T) {
		a, _ := newWalletAccount(t, pool)
		b, _ := newWalletAccount(t, pool)
		fund(t, pool, a, 1_000)
		err := post(ctx, pool, transferJournal(a, b, 1_001, uuid.NewString()))
		if !errors.Is(err, ledger.ErrInsufficientFunds) {
			t.Fatalf("got %v, want ErrInsufficientFunds", err)
		}
		if bal, _ := ledger.Balance(ctx, pool, a); bal != 1_000 {
			t.Errorf("balance changed after rejected posting: %d", bal)
		}
	})

	t.Run("unbalanced journal rejected by database even if app check bypassed", func(t *testing.T) {
		a, _ := newWalletAccount(t, pool)
		err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
			jid := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, posting_key, kind, reference_type, reference_id)
				VALUES ($1, $2, 'adjustment', 'test', $1)`, jid, uuid.NewString()); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (journal_id, account_id, currency, direction, amount)
				VALUES ($1, $2, 'KES', 'credit', 500), ($1, $3, 'KES', 'debit', 400)`, jid, a, ledger.AdjustmentsKES)
			return err
		})
		if err == nil {
			t.Fatal("unbalanced journal committed")
		}
		if bal, _ := ledger.Balance(ctx, pool, a); bal != 0 {
			t.Errorf("balance = %d after rolled-back unbalanced journal", bal)
		}
	})

	t.Run("posted entries are immutable", func(t *testing.T) {
		a, _ := newWalletAccount(t, pool)
		fund(t, pool, a, 500)
		if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET amount = 1 WHERE account_id = $1`, a); err == nil {
			t.Error("UPDATE ledger_entries succeeded")
		}
		if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries WHERE account_id = $1`, a); err == nil {
			t.Error("DELETE ledger_entries succeeded")
		}
		if _, err := pool.Exec(ctx, `DELETE FROM journal_transactions`); err == nil {
			t.Error("DELETE journal_transactions succeeded")
		}
	})

	t.Run("balance cannot be written directly", func(t *testing.T) {
		a, _ := newWalletAccount(t, pool)
		if _, err := pool.Exec(ctx, `UPDATE ledger_accounts SET balance = 1000000 WHERE id = $1`, a); err == nil {
			t.Fatal("direct balance overwrite succeeded")
		}
	})

	t.Run("currency mismatch rejected", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `INSERT INTO currencies (code, minor_units, name) VALUES ('USD', 2, 'US Dollar')`); err != nil {
			t.Fatal(err)
		}
		var usd uuid.UUID
		err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
			var err error
			usd, _, err = ledger.CreateWalletAccounts(ctx, tx, "USD")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		kes, _ := newWalletAccount(t, pool)
		fund(t, pool, kes, 1_000)
		// Posting KES entries against a USD account violates the composite FK.
		if err := post(ctx, pool, transferJournal(kes, usd, 100, uuid.NewString())); err == nil {
			t.Fatal("cross-currency posting succeeded")
		}
	})

	t.Run("reversal mirrors original and only once", func(t *testing.T) {
		a, _ := newWalletAccount(t, pool)
		b, _ := newWalletAccount(t, pool)
		fund(t, pool, a, 1_000)
		var jid uuid.UUID
		err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
			var err error
			jid, err = ledger.Post(ctx, tx, transferJournal(a, b, 600, uuid.NewString()))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		rev := func(key string) error {
			return db.WithTx(ctx, pool, func(tx pgx.Tx) error {
				_, err := ledger.Reverse(ctx, tx, jid, "transfer_reversal", key, "test reversal", nil)
				return err
			})
		}
		if err := rev("rev-" + jid.String()); err != nil {
			t.Fatal(err)
		}
		if bal, _ := ledger.Balance(ctx, pool, a); bal != 1_000 {
			t.Errorf("after reversal sender = %d, want 1000", bal)
		}
		if err := rev("rev2-" + jid.String()); err == nil {
			t.Error("second reversal of the same journal succeeded")
		}
		assertNoDrift(t, pool)
	})

	t.Run("concurrent spends cannot exceed balance", func(t *testing.T) {
		src, _ := newWalletAccount(t, pool)
		fund(t, pool, src, 10_000)
		const workers = 40
		dsts := make([]uuid.UUID, workers)
		for i := range dsts {
			dsts[i], _ = newWalletAccount(t, pool)
		}
		var ok, insufficient atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				err := post(ctx, pool, transferJournal(src, dsts[i], 1_000, fmt.Sprintf("conc-%s-%d", src, i)))
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ledger.ErrInsufficientFunds):
					insufficient.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}(i)
		}
		wg.Wait()
		if ok.Load() != 10 || insufficient.Load() != workers-10 {
			t.Fatalf("succeeded=%d insufficient=%d, want 10/%d", ok.Load(), insufficient.Load(), workers-10)
		}
		if bal, _ := ledger.Balance(ctx, pool, src); bal != 0 {
			t.Errorf("source balance = %d, want 0", bal)
		}
		assertNoDrift(t, pool)
	})

	t.Run("opposing transfers do not deadlock", func(t *testing.T) {
		a, _ := newWalletAccount(t, pool)
		b, _ := newWalletAccount(t, pool)
		fund(t, pool, a, 100_000)
		fund(t, pool, b, 100_000)
		var wg sync.WaitGroup
		errs := make(chan error, 200)
		for i := 0; i < 100; i++ {
			wg.Add(2)
			go func(i int) {
				defer wg.Done()
				errs <- post(ctx, pool, transferJournal(a, b, 10, fmt.Sprintf("ab-%s-%d", a, i)))
			}(i)
			go func(i int) {
				defer wg.Done()
				errs <- post(ctx, pool, transferJournal(b, a, 10, fmt.Sprintf("ba-%s-%d", b, i)))
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("opposing transfer failed: %v", err)
			}
		}
		ba, _ := ledger.Balance(ctx, pool, a)
		bb, _ := ledger.Balance(ctx, pool, b)
		if ba != 100_000 || bb != 100_000 {
			t.Errorf("balances = %d/%d, want 100000/100000", ba, bb)
		}
		assertNoDrift(t, pool)
	})
}
