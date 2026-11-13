// Package fixture builds users, wallets and balances for integration tests.
package fixture

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sovereigntrust/wallet/internal/auth"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/idempotency"
	"github.com/sovereigntrust/wallet/internal/ledger"
	"github.com/sovereigntrust/wallet/internal/money"
	"github.com/sovereigntrust/wallet/internal/wallet"
)

const PIN = "2580"
const Password = "correct-horse-battery"

type Env struct {
	Pool *pgxpool.Pool
	Auth *auth.Service
	Idem *idempotency.Store
	Log  *slog.Logger
}

func New(pool *pgxpool.Pool) *Env {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Env{
		Pool: pool,
		Auth: &auth.Service{
			Pool:       pool,
			Tokens:     &auth.TokenIssuer{Secret: []byte("test-secret-test-secret-test-secret!"), Issuer: "test", TTL: time.Minute},
			RefreshTTL: time.Hour, SMS: auth.LogSMS{Log: log}, OTPKey: []byte("otp-key"),
			RequirePhoneVerification: false, Log: log,
		},
		Idem: &idempotency.Store{Pool: pool},
		Log:  log,
	}
}

type User struct {
	ID     uuid.UUID
	Phone  string
	Wallet wallet.Wallet
}

// User registers an active user with a PIN and a KES wallet.
func (e *Env) User(t testing.TB, name string) User {
	t.Helper()
	ctx := context.Background()
	ph := fmt.Sprintf("+2547%08d", rand.IntN(100_000_000))
	u, err := e.Auth.Register(ctx, auth.RegisterInput{Phone: ph, FullName: name, Password: Password})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.Pool.Exec(ctx, `UPDATE users SET phone_verified_at = now(), status = 'active', kyc_tier = 3 WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Auth.SetPIN(ctx, u.ID, Password, PIN); err != nil {
		t.Fatalf("set pin: %v", err)
	}
	w, err := wallet.PrimaryForUser(ctx, e.Pool, u.ID, "KES")
	if err != nil {
		t.Fatal(err)
	}
	return User{ID: u.ID, Phone: u.Phone, Wallet: w}
}

// Fund credits a wallet from the M-Pesa clearing account (test-only shortcut).
func (e *Env) Fund(t testing.TB, w wallet.Wallet, major int64) {
	t.Helper()
	amount, _ := money.FromWholeMajor(major, money.KES)
	err := db.WithTx(context.Background(), e.Pool, func(tx pgx.Tx) error {
		_, err := ledger.Post(context.Background(), tx, ledger.Journal{
			PostingKey: "test-fund:" + uuid.NewString(), Kind: "adjustment", ReferenceType: "test", ReferenceID: uuid.New(),
			Currency: "KES", Entries: []ledger.Entry{
				{AccountID: ledger.AdjustmentsKES, Direction: ledger.Debit, Amount: amount},
				{AccountID: w.AvailableAccountID, Direction: ledger.Credit, Amount: amount},
			},
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *Env) Balance(t testing.TB, w wallet.Wallet) wallet.Balance {
	t.Helper()
	b, err := wallet.GetBalance(context.Background(), e.Pool, w)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// AssertLedgerConsistent checks projection drift and the trial balance.
func (e *Env) AssertLedgerConsistent(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	drift, err := ledger.Drift(ctx, e.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) > 0 {
		t.Fatalf("ledger projection drift: %+v", drift)
	}
	tb, err := ledger.TrialBalance(ctx, e.Pool)
	if err != nil {
		t.Fatal(err)
	}
	for c, v := range tb {
		if v != 0 {
			t.Fatalf("trial balance %s = %d", c, v)
		}
	}
}

func Key() string { return "test-" + uuid.NewString() }
