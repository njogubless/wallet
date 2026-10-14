package withdrawals_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/callbacks"
	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/ledger"
	"github.com/sovereigntrust/wallet/internal/payments"
	"github.com/sovereigntrust/wallet/internal/payments/fake"
	"github.com/sovereigntrust/wallet/internal/testutil"
	"github.com/sovereigntrust/wallet/internal/testutil/fixture"
	"github.com/sovereigntrust/wallet/internal/withdrawals"
)

type harness struct {
	env  *fixture.Env
	prov *fake.Provider
	svc  *withdrawals.Service
	cb   *callbacks.Store
	proc *callbacks.Processor
}

func newHarness(t *testing.T, payoutQuery bool) *harness {
	pool := testutil.NewDB(t)
	env := fixture.New(pool)
	prov := fake.New("mpesa")
	prov.Caps.PayoutStatusQuery = payoutQuery
	svc := &withdrawals.Service{Pool: pool, Providers: payments.Registry{"mpesa": prov}, Idem: env.Idem, Auth: env.Auth, Log: env.Log}
	dsvc := &deposits.Service{Pool: pool, Providers: payments.Registry{"mpesa": prov}, Idem: env.Idem, Log: env.Log}
	return &harness{env: env, prov: prov, svc: svc, cb: &callbacks.Store{Pool: pool},
		proc: &callbacks.Processor{Pool: pool, Deposits: dsvc, Withdrawals: svc, Log: env.Log}}
}

func (h *harness) create(t *testing.T, u fixture.User, amount string) *withdrawals.Withdrawal {
	t.Helper()
	w, _, err := h.svc.Create(context.Background(), withdrawals.CreateInput{UserID: u.ID, Provider: "mpesa", Amount: amount, PIN: fixture.PIN, IdempotencyKey: fixture.Key()})
	if err != nil {
		t.Fatalf("create withdrawal: %v", err)
	}
	return w
}

func get(t *testing.T, h *harness, id uuid.UUID) *withdrawals.Withdrawal {
	t.Helper()
	w, err := withdrawals.Get(context.Background(), h.env.Pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// Default withdrawal fee rule: 15.00 + 0.5%, min 15.00, max 300.00.
func TestWithdrawalsWithStatusQuery(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	t.Run("hold then settle", func(t *testing.T) {
		u := h.env.User(t, "Wd One")
		h.env.Fund(t, u.Wallet, 2_000)
		w := h.create(t, u, "1000") // fee 15 + 5 = 20.00
		if w.Status != "pending" || w.Fee != 2_000 {
			t.Fatalf("created %+v", w)
		}
		b := h.env.Balance(t, u.Wallet)
		if b.Available != 98_000 || b.Held != 102_000 {
			t.Fatalf("after hold: %+v", b)
		}
		if err := h.svc.Dispatch(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomeSucceeded, Receipt: "B2C" + w.ID.String()[:6]}, nil)
		defer h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomePending}, nil)
		if err := h.svc.Poll(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		w = get(t, h, w.ID)
		if w.Status != "succeeded" {
			t.Fatalf("status %s", w.Status)
		}
		b = h.env.Balance(t, u.Wallet)
		if b.Available != 98_000 || b.Held != 0 {
			t.Fatalf("after settle: %+v", b)
		}
		h.env.AssertLedgerConsistent(t)
	})

	t.Run("insufficient funds including fee", func(t *testing.T) {
		u := h.env.User(t, "Wd Two")
		h.env.Fund(t, u.Wallet, 1_000)
		_, _, err := h.svc.Create(ctx, withdrawals.CreateInput{UserID: u.ID, Provider: "mpesa", Amount: "1000", PIN: fixture.PIN, IdempotencyKey: fixture.Key()})
		if !apperr.Is(err, apperr.CodeInsufficientFunds) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("rejection releases funds", func(t *testing.T) {
		u := h.env.User(t, "Wd Three")
		h.env.Fund(t, u.Wallet, 500)
		w := h.create(t, u, "100")
		h.prov.SetInitErr(&payments.ProviderError{Class: payments.ErrRejected, Code: "400", Message: "invalid"})
		defer h.prov.SetInitErr(nil)
		if err := h.svc.Dispatch(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		if w = get(t, h, w.ID); w.Status != "failed" {
			t.Fatalf("status %s", w.Status)
		}
		if b := h.env.Balance(t, u.Wallet); b.Available != 50_000 || b.Held != 0 {
			t.Fatalf("after release %+v", b)
		}
	})

	t.Run("timeout keeps funds held, never resends, admin resolves", func(t *testing.T) {
		u := h.env.User(t, "Wd Four")
		h.env.Fund(t, u.Wallet, 500)
		w := h.create(t, u, "100")
		h.prov.SetInitErr(&payments.ProviderError{Class: payments.ErrUncertain, Message: "read timeout"})
		if err := h.svc.Dispatch(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		h.prov.SetInitErr(nil)
		w = get(t, h, w.ID)
		if w.Status != "processing" || !w.OutcomeUncertain {
			t.Fatalf("after timeout: %s uncertain=%v", w.Status, w.OutcomeUncertain)
		}
		before := h.prov.Initiations.Load()
		for i := 0; i < 3; i++ {
			_ = h.svc.Dispatch(ctx, w.ID) // retries of the job must not resend
		}
		if h.prov.Initiations.Load() != before {
			t.Fatal("payout resent after uncertain outcome")
		}
		if b := h.env.Balance(t, u.Wallet); b.Held != w.Total() {
			t.Fatalf("funds not held: %+v", b)
		}
		// Provider still cannot say: stays held.
		h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomeUnknown}, nil)
		_ = h.svc.Poll(ctx, w.ID)
		if w = get(t, h, w.ID); w.Status != "processing" {
			t.Fatalf("status %s", w.Status)
		}
		h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomePending}, nil)
		if _, err := h.svc.Resolve(ctx, w.ID, "failed", "", "short"); !apperr.Is(err, apperr.CodeValidation) {
			t.Fatalf("resolve without evidence: %v", err)
		}
		if _, err := h.svc.Resolve(ctx, w.ID, "failed", "", "Provider portal statement shows no payout for this reference"); err != nil {
			t.Fatal(err)
		}
		if b := h.env.Balance(t, u.Wallet); b.Available != 50_000 || b.Held != 0 {
			t.Fatalf("after resolve %+v", b)
		}
		h.env.AssertLedgerConsistent(t)
	})

	t.Run("not-sent returns to pending for retry", func(t *testing.T) {
		u := h.env.User(t, "Wd Five")
		h.env.Fund(t, u.Wallet, 500)
		w := h.create(t, u, "100")
		h.prov.SetInitErr(&payments.ProviderError{Class: payments.ErrNotSent, Message: "dns"})
		if err := h.svc.Dispatch(ctx, w.ID); err == nil {
			t.Fatal("expected retry error")
		}
		h.prov.SetInitErr(nil)
		if w = get(t, h, w.ID); w.Status != "pending" {
			t.Fatalf("status %s", w.Status)
		}
		if err := h.svc.Dispatch(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		if w = get(t, h, w.ID); w.Status != "processing" || w.ProviderStatus != "accepted" {
			t.Fatalf("after retry %s/%s", w.Status, w.ProviderStatus)
		}
	})

	t.Run("crash mid-send marks uncertain without resend", func(t *testing.T) {
		u := h.env.User(t, "Wd Six")
		h.env.Fund(t, u.Wallet, 500)
		w := h.create(t, u, "100")
		if _, err := h.env.Pool.Exec(ctx, `UPDATE withdrawals SET status = 'processing', provider_status = 'sending' WHERE id = $1`, w.ID); err != nil {
			t.Fatal(err)
		}
		before := h.prov.Initiations.Load()
		if err := h.svc.Dispatch(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		if h.prov.Initiations.Load() != before {
			t.Fatal("resent")
		}
		if w = get(t, h, w.ID); !w.OutcomeUncertain || w.Status != "processing" {
			t.Fatalf("got %s uncertain=%v", w.Status, w.OutcomeUncertain)
		}
	})

	t.Run("concurrent withdrawals cannot overspend", func(t *testing.T) {
		u := h.env.User(t, "Wd Seven")
		h.env.Fund(t, u.Wallet, 1_000) // each 100 + 15.50 fee = 115.50 => 8 fit
		var ok, insufficient atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 15; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, err := h.svc.Create(ctx, withdrawals.CreateInput{UserID: u.ID, Provider: "mpesa", Amount: "100", PIN: fixture.PIN, IdempotencyKey: fixture.Key()})
				switch {
				case err == nil:
					ok.Add(1)
				case apperr.Is(err, apperr.CodeInsufficientFunds):
					insufficient.Add(1)
				default:
					t.Errorf("unexpected %v", err)
				}
			}()
		}
		wg.Wait()
		if ok.Load() != 8 {
			t.Fatalf("ok=%d insufficient=%d, want 8 successes", ok.Load(), insufficient.Load())
		}
		b := h.env.Balance(t, u.Wallet)
		if b.Available != 100_000-8*11_550 || b.Held != 8*11_550 {
			t.Fatalf("balances %+v", b)
		}
		h.env.AssertLedgerConsistent(t)
	})
}

func b2cResult(conv, orig string, code int, amountMajor int, txid string) []byte {
	return []byte(fmt.Sprintf(`{"Result":{"ResultType":0,"ResultCode":%d,"ResultDesc":"d","OriginatorConversationID":%q,
		"ConversationID":%q,"TransactionID":%q,"ResultParameters":{"ResultParameter":[{"Key":"TransactionAmount","Value":%d}]}}}`,
		code, orig, conv, txid, amountMajor))
}

func (h *harness) deliver(t *testing.T, kind string, raw []byte) string {
	t.Helper()
	ctx := context.Background()
	dup, err := h.cb.Save(ctx, "mpesa", kind, raw, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		return "duplicate"
	}
	var id uuid.UUID
	if err := h.env.Pool.QueryRow(ctx, `SELECT id FROM provider_callbacks ORDER BY received_at DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := h.proc.Process(ctx, id); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = h.env.Pool.QueryRow(ctx, `SELECT status FROM provider_callbacks WHERE id = $1`, id).Scan(&status)
	return status
}

// Daraja-like provider: no payout status API, results arrive only by callback.
func TestWithdrawalsResultCallbacks(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()

	dispatched := func(t *testing.T, name string) (fixture.User, *withdrawals.Withdrawal) {
		u := h.env.User(t, name)
		h.env.Fund(t, u.Wallet, 500)
		w := h.create(t, u, "100")
		if err := h.svc.Dispatch(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		return u, get(t, h, w.ID)
	}

	t.Run("matched success callback settles once", func(t *testing.T) {
		u, w := dispatched(t, "Cb One")
		raw := b2cResult(*w.ProviderConvRef, *w.ProviderRequestRef, 0, 100, "TX"+w.ID.String()[:6])
		if s := h.deliver(t, callbacks.KindB2CResult, raw); s != "processed" {
			t.Fatalf("status %s", s)
		}
		if s := h.deliver(t, callbacks.KindB2CResult, raw); s != "duplicate" {
			t.Fatalf("duplicate not detected: %s", s)
		}
		if w = get(t, h, w.ID); w.Status != "succeeded" {
			t.Fatalf("status %s", w.Status)
		}
		if b := h.env.Balance(t, u.Wallet); b.Held != 0 || b.Available != 50_000-11_550 {
			t.Fatalf("balances %+v", b)
		}
	})

	t.Run("mismatched amount is flagged, not applied", func(t *testing.T) {
		u, w := dispatched(t, "Cb Two")
		raw := b2cResult(*w.ProviderConvRef, *w.ProviderRequestRef, 2001, 999, "")
		if s := h.deliver(t, callbacks.KindB2CResult, raw); s != "ignored" {
			t.Fatalf("status %s", s)
		}
		w = get(t, h, w.ID)
		if w.Status != "processing" || !w.NeedsReview {
			t.Fatalf("status %s review %v", w.Status, w.NeedsReview)
		}
		if b := h.env.Balance(t, u.Wallet); b.Held != w.Total() {
			t.Fatal("funds released on unverified callback")
		}
	})

	t.Run("forged identifiers are ignored", func(t *testing.T) {
		_, w := dispatched(t, "Cb Three")
		raw := b2cResult("AG_FORGED", *w.ProviderRequestRef, 1, 100, "")
		h.deliver(t, callbacks.KindB2CResult, raw)
		if w = get(t, h, w.ID); w.Status != "processing" {
			t.Fatalf("status %s", w.Status)
		}
	})

	t.Run("matched failure releases; later success flagged not flipped", func(t *testing.T) {
		u, w := dispatched(t, "Cb Four")
		h.deliver(t, callbacks.KindB2CResult, b2cResult(*w.ProviderConvRef, *w.ProviderRequestRef, 2001, 100, ""))
		if w = get(t, h, w.ID); w.Status != "failed" {
			t.Fatalf("status %s", w.Status)
		}
		h.deliver(t, callbacks.KindB2CResult, b2cResult(*w.ProviderConvRef, *w.ProviderRequestRef, 0, 100, "LATE1"))
		w = get(t, h, w.ID)
		if w.Status != "failed" || !w.NeedsReview {
			t.Fatalf("conflict handling: %s review=%v", w.Status, w.NeedsReview)
		}
		if b := h.env.Balance(t, u.Wallet); b.Available != 50_000 {
			t.Fatalf("balance %+v", b)
		}
		var n int
		_ = h.env.Pool.QueryRow(ctx, `SELECT count(*) FROM journal_transactions WHERE reference_id = $1`, w.ID).Scan(&n)
		if n != 2 { // hold + release only
			t.Fatalf("journals = %d", n)
		}
		h.env.AssertLedgerConsistent(t)
	})

	t.Run("queue timeout keeps funds held", func(t *testing.T) {
		u, w := dispatched(t, "Cb Five")
		h.deliver(t, callbacks.KindB2CTimeout, b2cResult(*w.ProviderConvRef, *w.ProviderRequestRef, 1, 100, ""))
		w = get(t, h, w.ID)
		if w.Status != "processing" || !w.OutcomeUncertain {
			t.Fatalf("status %s uncertain %v", w.Status, w.OutcomeUncertain)
		}
		if b := h.env.Balance(t, u.Wallet); b.Held != w.Total() {
			t.Fatal("funds released on timeout")
		}
	})

	t.Run("reversal after success returns principal", func(t *testing.T) {
		u, w := dispatched(t, "Cb Six")
		h.deliver(t, callbacks.KindB2CResult, b2cResult(*w.ProviderConvRef, *w.ProviderRequestRef, 0, 100, "TXR"+w.ID.String()[:5]))
		tx, err := h.env.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := withdrawals.Reverse(ctx, tx, w.ID, u.ID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if bal, _ := ledger.Balance(ctx, h.env.Pool, u.Wallet.AvailableAccountID); bal != 50_000-11_550+10_000 {
			t.Fatalf("balance %d", bal)
		}
		h.env.AssertLedgerConsistent(t)
	})
}
