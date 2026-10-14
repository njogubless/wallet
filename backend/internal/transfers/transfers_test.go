package transfers_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/testutil"
	"github.com/sovereigntrust/wallet/internal/testutil/fixture"
	"github.com/sovereigntrust/wallet/internal/transfers"
)

func TestMaskName(t *testing.T) {
	cases := map[string]string{"Amina Otieno": "Amina O.", "Juma": "Juma", "  Brian  K  Kip ": "Brian K.", "": ""}
	for in, want := range cases {
		if got := transfers.MaskName(in); got != want {
			t.Errorf("MaskName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTransfersIntegration(t *testing.T) {
	pool := testutil.NewDB(t)
	env := fixture.New(pool)
	svc := &transfers.Service{Pool: pool, Auth: env.Auth, Idem: env.Idem}
	ctx := context.Background()

	t.Run("successful transfer moves money atomically", func(t *testing.T) {
		alice, bob := env.User(t, "Alice Wanjiru"), env.User(t, "Bob Otieno")
		env.Fund(t, alice.Wallet, 1_000)
		tr, replay, err := svc.Create(ctx, transfers.CreateInput{UserID: alice.ID, RecipientPhone: bob.Phone,
			Amount: "250.50", Description: "lunch", PIN: fixture.PIN, IdempotencyKey: fixture.Key()})
		if err != nil || replay {
			t.Fatalf("create: %v replay=%v", err, replay)
		}
		if tr.Status != "succeeded" || tr.Amount != 25_050 {
			t.Fatalf("unexpected transfer %+v", tr)
		}
		if b := env.Balance(t, alice.Wallet); b.Available != 74_950 {
			t.Errorf("alice available = %d", b.Available)
		}
		if b := env.Balance(t, bob.Wallet); b.Available != 25_050 {
			t.Errorf("bob available = %d", b.Available)
		}
		env.AssertLedgerConsistent(t)
	})

	t.Run("insufficient funds rejected and key released", func(t *testing.T) {
		alice, bob := env.User(t, "Alice A"), env.User(t, "Bob B")
		env.Fund(t, alice.Wallet, 100)
		key := fixture.Key()
		in := transfers.CreateInput{UserID: alice.ID, RecipientPhone: bob.Phone, Amount: "100.01", PIN: fixture.PIN, IdempotencyKey: key}
		if _, _, err := svc.Create(ctx, in); !apperr.Is(err, apperr.CodeInsufficientFunds) {
			t.Fatalf("got %v, want insufficient_funds", err)
		}
		// After topping up, the same logical request with the same key may proceed.
		env.Fund(t, alice.Wallet, 1)
		if _, _, err := svc.Create(ctx, in); err != nil {
			t.Fatalf("retry after top-up: %v", err)
		}
	})

	t.Run("wrong PIN rejected", func(t *testing.T) {
		alice, bob := env.User(t, "Alice C"), env.User(t, "Bob D")
		env.Fund(t, alice.Wallet, 100)
		_, _, err := svc.Create(ctx, transfers.CreateInput{UserID: alice.ID, RecipientPhone: bob.Phone, Amount: "10", PIN: "9137", IdempotencyKey: fixture.Key()})
		if !apperr.Is(err, apperr.CodeInvalidPIN) {
			t.Fatalf("got %v, want invalid_pin", err)
		}
		if b := env.Balance(t, alice.Wallet); b.Available != 10_000 {
			t.Errorf("balance changed: %d", b.Available)
		}
	})

	t.Run("same idempotency key replays without double posting", func(t *testing.T) {
		alice, bob := env.User(t, "Alice E"), env.User(t, "Bob F")
		env.Fund(t, alice.Wallet, 1_000)
		in := transfers.CreateInput{UserID: alice.ID, RecipientAccount: bob.Wallet.AccountNumber, Amount: "100", PIN: fixture.PIN, IdempotencyKey: fixture.Key()}
		first, _, err := svc.Create(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		second, replay, err := svc.Create(ctx, in)
		if err != nil || !replay || second.ID != first.ID {
			t.Fatalf("replay: id=%v replay=%v err=%v", second.ID, replay, err)
		}
		if b := env.Balance(t, alice.Wallet); b.Available != 90_000 {
			t.Errorf("double debit: available = %d", b.Available)
		}
	})

	t.Run("idempotency key reuse with different payload rejected", func(t *testing.T) {
		alice, bob := env.User(t, "Alice G"), env.User(t, "Bob H")
		env.Fund(t, alice.Wallet, 1_000)
		key := fixture.Key()
		if _, _, err := svc.Create(ctx, transfers.CreateInput{UserID: alice.ID, RecipientPhone: bob.Phone, Amount: "100", PIN: fixture.PIN, IdempotencyKey: key}); err != nil {
			t.Fatal(err)
		}
		_, _, err := svc.Create(ctx, transfers.CreateInput{UserID: alice.ID, RecipientPhone: bob.Phone, Amount: "200", PIN: fixture.PIN, IdempotencyKey: key})
		if !apperr.Is(err, apperr.CodeIdempotencyMismatch) {
			t.Fatalf("got %v, want idempotency mismatch", err)
		}
	})

	t.Run("concurrent duplicates with one key execute once", func(t *testing.T) {
		alice, bob := env.User(t, "Alice I"), env.User(t, "Bob J")
		env.Fund(t, alice.Wallet, 1_000)
		in := transfers.CreateInput{UserID: alice.ID, RecipientPhone: bob.Phone, Amount: "100", PIN: fixture.PIN, IdempotencyKey: fixture.Key()}
		var wg sync.WaitGroup
		var created, replays, inflight atomic.Int64
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, replay, err := svc.Create(ctx, in)
				switch {
				case err == nil && !replay:
					created.Add(1)
				case err == nil && replay:
					replays.Add(1)
				case apperr.Is(err, apperr.CodeIdempotencyInFlight):
					inflight.Add(1)
				default:
					t.Errorf("unexpected: %v", err)
				}
			}()
		}
		wg.Wait()
		if created.Load() != 1 {
			t.Fatalf("created = %d (replays %d, in-flight %d), want exactly 1", created.Load(), replays.Load(), inflight.Load())
		}
		if b := env.Balance(t, bob.Wallet); b.Available != 10_000 {
			t.Fatalf("recipient credited %d, want 10000", b.Available)
		}
	})

	t.Run("concurrent distinct transfers cannot overspend", func(t *testing.T) {
		alice := env.User(t, "Alice K")
		bob := env.User(t, "Bob L")
		env.Fund(t, alice.Wallet, 1_000)
		var wg sync.WaitGroup
		var ok, insufficient atomic.Int64
		for i := 0; i < 25; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, err := svc.Create(ctx, transfers.CreateInput{UserID: alice.ID, RecipientPhone: bob.Phone, Amount: "100", PIN: fixture.PIN, IdempotencyKey: fixture.Key()})
				switch {
				case err == nil:
					ok.Add(1)
				case apperr.Is(err, apperr.CodeInsufficientFunds):
					insufficient.Add(1)
				default:
					t.Errorf("unexpected: %v", err)
				}
			}()
		}
		wg.Wait()
		if ok.Load() != 10 || insufficient.Load() != 15 {
			t.Fatalf("ok=%d insufficient=%d, want 10/15", ok.Load(), insufficient.Load())
		}
		if b := env.Balance(t, alice.Wallet); b.Available != 0 {
			t.Fatalf("alice available = %d, want 0", b.Available)
		}
		env.AssertLedgerConsistent(t)
	})

	t.Run("daily limit enforced under concurrency", func(t *testing.T) {
		alice, bob := env.User(t, "Alice M"), env.User(t, "Bob N")
		if _, err := pool.Exec(ctx, `UPDATE users SET kyc_tier = 1 WHERE id = $1`, alice.ID); err != nil {
			t.Fatal(err)
		}
		env.Fund(t, alice.Wallet, 500_000)
		// Tier 1 defaults: per-transaction 70,000, daily 150,000 => at most 2 of 70,000.
		var wg sync.WaitGroup
		var ok, limited atomic.Int64
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, err := svc.Create(ctx, transfers.CreateInput{UserID: alice.ID, RecipientPhone: bob.Phone, Amount: "70000", PIN: fixture.PIN, IdempotencyKey: fixture.Key()})
				switch {
				case err == nil:
					ok.Add(1)
				case apperr.Is(err, apperr.CodeLimitExceeded):
					limited.Add(1)
				default:
					t.Errorf("unexpected: %v", err)
				}
			}()
		}
		wg.Wait()
		if ok.Load() != 2 || limited.Load() != 4 {
			t.Fatalf("ok=%d limited=%d, want 2/4", ok.Load(), limited.Load())
		}
	})

	t.Run("self transfer and restricted sender rejected", func(t *testing.T) {
		alice := env.User(t, "Alice O")
		env.Fund(t, alice.Wallet, 100)
		if _, _, err := svc.Create(ctx, transfers.CreateInput{UserID: alice.ID, RecipientPhone: alice.Phone, Amount: "10", PIN: fixture.PIN, IdempotencyKey: fixture.Key()}); !apperr.Is(err, apperr.CodeValidation) {
			t.Fatalf("self transfer: %v", err)
		}
		bob := env.User(t, "Bob P")
		if _, err := pool.Exec(ctx, `UPDATE users SET status = 'restricted' WHERE id = $1`, alice.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := svc.Create(ctx, transfers.CreateInput{UserID: alice.ID, RecipientPhone: bob.Phone, Amount: "10", PIN: fixture.PIN, IdempotencyKey: fixture.Key()}); !apperr.Is(err, apperr.CodeAccountRestricted) {
			t.Fatalf("restricted sender: %v", err)
		}
	})

	t.Run("reversal restores balances via compensating journal", func(t *testing.T) {
		alice, bob := env.User(t, "Alice Q"), env.User(t, "Bob R")
		env.Fund(t, alice.Wallet, 500)
		tr, _, err := svc.Create(ctx, transfers.CreateInput{UserID: alice.ID, RecipientPhone: bob.Phone, Amount: "200", PIN: fixture.PIN, IdempotencyKey: fixture.Key()})
		if err != nil {
			t.Fatal(err)
		}
		err = db.WithTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := transfers.Reverse(ctx, tx, tr.ID, alice.ID, true)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if b := env.Balance(t, alice.Wallet); b.Available != 50_000 {
			t.Errorf("alice after reversal = %d", b.Available)
		}
		if b := env.Balance(t, bob.Wallet); b.Available != 0 {
			t.Errorf("bob after reversal = %d", b.Available)
		}
		got, _ := transfers.Get(ctx, pool, tr.ID)
		if got.Status != "reversed" {
			t.Errorf("status = %s", got.Status)
		}
		env.AssertLedgerConsistent(t)
	})
}
