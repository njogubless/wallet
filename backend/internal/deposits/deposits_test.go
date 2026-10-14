package deposits_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/callbacks"
	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/ledger"
	"github.com/sovereigntrust/wallet/internal/outbox"
	"github.com/sovereigntrust/wallet/internal/payments"
	"github.com/sovereigntrust/wallet/internal/payments/fake"
	"github.com/sovereigntrust/wallet/internal/testutil"
	"github.com/sovereigntrust/wallet/internal/testutil/fixture"
	"github.com/sovereigntrust/wallet/internal/withdrawals"
)

func TestTransitions(t *testing.T) {
	legal := [][2]string{{"created", "pending_provider"}, {"pending_provider", "succeeded"}, {"pending_provider", "expired"},
		{"expired", "succeeded"}, {"succeeded", "reversed"}, {"created", "failed"}}
	for _, tr := range legal {
		if !deposits.CanTransition(tr[0], tr[1]) {
			t.Errorf("%s -> %s should be legal", tr[0], tr[1])
		}
	}
	illegal := [][2]string{{"failed", "succeeded"}, {"succeeded", "failed"}, {"reversed", "succeeded"}, {"created", "succeeded"}, {"succeeded", "succeeded"}}
	for _, tr := range illegal {
		if deposits.CanTransition(tr[0], tr[1]) {
			t.Errorf("%s -> %s should be illegal", tr[0], tr[1])
		}
	}
}

type harness struct {
	env  *fixture.Env
	prov *fake.Provider
	svc  *deposits.Service
	cb   *callbacks.Store
	proc *callbacks.Processor
}

func newHarness(t *testing.T) *harness {
	pool := testutil.NewDB(t)
	env := fixture.New(pool)
	prov := fake.New("mpesa")
	svc := &deposits.Service{Pool: pool, Providers: payments.Registry{"mpesa": prov}, Idem: env.Idem, Expiry: 3 * time.Minute, Log: env.Log}
	wsvc := &withdrawals.Service{Pool: pool, Providers: payments.Registry{"mpesa": prov}, Idem: env.Idem, Auth: env.Auth, Log: env.Log}
	return &harness{env: env, prov: prov, svc: svc, cb: &callbacks.Store{Pool: pool},
		proc: &callbacks.Processor{Pool: pool, Deposits: svc, Withdrawals: wsvc, Log: env.Log}}
}

func (h *harness) create(t *testing.T, u fixture.User, amount string) *deposits.Deposit {
	t.Helper()
	d, _, err := h.svc.Create(context.Background(), deposits.CreateInput{UserID: u.ID, Provider: "mpesa", Phone: u.Phone, Amount: amount, IdempotencyKey: fixture.Key()})
	if err != nil {
		t.Fatalf("create deposit: %v", err)
	}
	return d
}

func get(t *testing.T, h *harness, id uuid.UUID) *deposits.Deposit {
	t.Helper()
	d, err := deposits.Get(context.Background(), h.env.Pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func isRetry(err error) bool {
	var r *outbox.RetryError
	return errors.As(err, &r)
}

func stkCallback(checkout, code string, amountMajor int, receipt string) []byte {
	if code != "0" {
		return []byte(fmt.Sprintf(`{"Body":{"stkCallback":{"MerchantRequestID":"M-1","CheckoutRequestID":%q,"ResultCode":%s,"ResultDesc":"x"}}}`, checkout, code))
	}
	return []byte(fmt.Sprintf(`{"Body":{"stkCallback":{"MerchantRequestID":"M-1","CheckoutRequestID":%q,"ResultCode":0,"ResultDesc":"ok",
		"CallbackMetadata":{"Item":[{"Name":"Amount","Value":%d},{"Name":"MpesaReceiptNumber","Value":%q},{"Name":"PhoneNumber","Value":254700000000}]}}}}`,
		checkout, amountMajor, receipt))
}

func (h *harness) deliver(t *testing.T, raw []byte) {
	t.Helper()
	ctx := context.Background()
	dup, err := h.cb.Save(ctx, "mpesa", callbacks.KindSTK, raw, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		return
	}
	var id uuid.UUID
	if err := h.env.Pool.QueryRow(ctx, `SELECT id FROM provider_callbacks ORDER BY received_at DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := h.proc.Process(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestDepositsIntegration(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	t.Run("happy path credits exactly once", func(t *testing.T) {
		u := h.env.User(t, "Dep One")
		d := h.create(t, u, "5000")
		if d.Status != "created" {
			t.Fatalf("status %s", d.Status)
		}
		if b := h.env.Balance(t, u.Wallet); b.Available != 0 {
			t.Fatal("credited before confirmation")
		}
		if err := h.svc.Dispatch(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		d = get(t, h, d.ID)
		if d.Status != "pending_provider" || d.ProviderStatus != "accepted" {
			t.Fatalf("after dispatch: %s/%s", d.Status, d.ProviderStatus)
		}
		// still pending at provider
		if err := h.svc.Poll(ctx, d.ID); !isRetry(err) {
			t.Fatalf("pending poll: %v", err)
		}
		if b := h.env.Balance(t, u.Wallet); b.Available != 0 {
			t.Fatal("credited while pending")
		}
		h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomeSucceeded, Receipt: "RCPT" + d.ID.String()[:6], ResultCode: "0"}, nil)
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ { // concurrent confirmations
			wg.Add(1)
			go func() { defer wg.Done(); _ = h.svc.Poll(ctx, d.ID) }()
		}
		wg.Wait()
		d = get(t, h, d.ID)
		if d.Status != "succeeded" || d.ProviderReceipt == nil {
			t.Fatalf("after confirm: %+v", d)
		}
		if b := h.env.Balance(t, u.Wallet); b.Available != 500_000 {
			t.Fatalf("available = %d, want 500000", b.Available)
		}
		// Redispatch is a no-op.
		before := h.prov.Initiations.Load()
		_ = h.svc.Dispatch(ctx, d.ID)
		if h.prov.Initiations.Load() != before {
			t.Fatal("dispatch resent a completed deposit")
		}
		h.env.AssertLedgerConsistent(t)
		h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomePending}, nil)
	})

	t.Run("idempotent create replays", func(t *testing.T) {
		u := h.env.User(t, "Dep Two")
		in := deposits.CreateInput{UserID: u.ID, Provider: "mpesa", Phone: u.Phone, Amount: "100", IdempotencyKey: fixture.Key()}
		a, _, err := h.svc.Create(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		b, replay, err := h.svc.Create(ctx, in)
		if err != nil || !replay || a.ID != b.ID {
			t.Fatalf("replay %v %v %v", b.ID, replay, err)
		}
		in.Amount = "200"
		if _, _, err := h.svc.Create(ctx, in); !apperr.Is(err, apperr.CodeIdempotencyMismatch) {
			t.Fatalf("mismatch: %v", err)
		}
	})

	t.Run("fractional shillings rejected", func(t *testing.T) {
		u := h.env.User(t, "Dep Frac")
		_, _, err := h.svc.Create(ctx, deposits.CreateInput{UserID: u.ID, Provider: "mpesa", Phone: u.Phone, Amount: "100.50", IdempotencyKey: fixture.Key()})
		if !apperr.Is(err, apperr.CodeValidation) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("provider rejection fails without credit", func(t *testing.T) {
		u := h.env.User(t, "Dep Three")
		d := h.create(t, u, "100")
		h.prov.SetInitErr(&payments.ProviderError{Class: payments.ErrRejected, Code: "400.002.02", Message: "Invalid PhoneNumber"})
		defer h.prov.SetInitErr(nil)
		if err := h.svc.Dispatch(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if d = get(t, h, d.ID); d.Status != "failed" {
			t.Fatalf("status %s", d.Status)
		}
	})

	t.Run("not-sent error is retried safely", func(t *testing.T) {
		u := h.env.User(t, "Dep Four")
		d := h.create(t, u, "100")
		h.prov.SetInitErr(&payments.ProviderError{Class: payments.ErrNotSent, Message: "dial tcp: refused"})
		if err := h.svc.Dispatch(ctx, d.ID); err == nil {
			t.Fatal("expected retryable error")
		}
		if d = get(t, h, d.ID); d.Status != "created" || d.ProviderStatus != "not_sent" {
			t.Fatalf("after not-sent: %s/%s", d.Status, d.ProviderStatus)
		}
		h.prov.SetInitErr(nil)
		if err := h.svc.Dispatch(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if d = get(t, h, d.ID); d.Status != "pending_provider" {
			t.Fatalf("after retry: %s", d.Status)
		}
	})

	t.Run("uncertain dispatch stays pending and resolves via status query", func(t *testing.T) {
		u := h.env.User(t, "Dep Five")
		d := h.create(t, u, "100")
		h.prov.SetInitErr(&payments.ProviderError{Class: payments.ErrUncertain, Message: "timeout"})
		if err := h.svc.Dispatch(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		h.prov.SetInitErr(nil)
		if d = get(t, h, d.ID); d.Status != "pending_provider" || d.ProviderStatus != "unknown" {
			t.Fatalf("after uncertain: %s/%s", d.Status, d.ProviderStatus)
		}
		h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomeSucceeded}, nil)
		defer h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomePending}, nil)
		if err := h.svc.Poll(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if d = get(t, h, d.ID); d.Status != "succeeded" {
			t.Fatalf("status %s", d.Status)
		}
	})

	t.Run("crash mid-send never resends", func(t *testing.T) {
		u := h.env.User(t, "Dep Six")
		d := h.create(t, u, "100")
		if _, err := h.env.Pool.Exec(ctx, `UPDATE deposits SET provider_status = 'sending' WHERE id = $1`, d.ID); err != nil {
			t.Fatal(err)
		}
		before := h.prov.Initiations.Load()
		if err := h.svc.Dispatch(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if h.prov.Initiations.Load() != before {
			t.Fatal("resent after crash")
		}
		if d = get(t, h, d.ID); d.ProviderStatus != "unknown" || d.Status != "pending_provider" {
			t.Fatalf("after crash recovery: %s/%s", d.Status, d.ProviderStatus)
		}
	})

	t.Run("callbacks are hints: query decides, duplicates ignored", func(t *testing.T) {
		u := h.env.User(t, "Dep Seven")
		d := h.create(t, u, "300")
		if err := h.svc.Dispatch(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		d = get(t, h, d.ID)
		// A forged "success" callback while the provider says it is still pending: no credit.
		h.deliver(t, stkCallback(*d.ProviderCheckoutRef, "0", 300, "FORGED1"))
		if b := h.env.Balance(t, u.Wallet); b.Available != 0 {
			t.Fatal("credited on unconfirmed callback")
		}
		// Provider confirms; a (late, out-of-order) failure callback must not undo it.
		h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomeSucceeded, ResultCode: "0"}, nil)
		h.deliver(t, stkCallback(*d.ProviderCheckoutRef, "1032", 0, ""))
		h.deliver(t, stkCallback(*d.ProviderCheckoutRef, "1032", 0, "")) // duplicate
		h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomeFailed, ResultCode: "1032"}, nil)
		h.deliver(t, stkCallback(*d.ProviderCheckoutRef, "1", 0, ""))
		h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomePending}, nil)
		d = get(t, h, d.ID)
		if d.Status != "succeeded" {
			t.Fatalf("status %s", d.Status)
		}
		if b := h.env.Balance(t, u.Wallet); b.Available != 30_000 {
			t.Fatalf("available %d", b.Available)
		}
		var n int
		_ = h.env.Pool.QueryRow(ctx, `SELECT count(*) FROM provider_callbacks WHERE dedupe_key LIKE $1`, *d.ProviderCheckoutRef+"%").Scan(&n)
		if n != 3 {
			t.Fatalf("stored callbacks = %d, want 3 (duplicate dropped)", n)
		}
		// Receipt was taken from the earlier success callback.
		if d.ProviderReceipt == nil || *d.ProviderReceipt != "FORGED1" {
			t.Logf("receipt label = %v", d.ProviderReceipt)
		}
		h.env.AssertLedgerConsistent(t)
	})

	t.Run("callback for unknown checkout is ignored", func(t *testing.T) {
		h.deliver(t, stkCallback("ws_CO_unknown", "0", 100, "ZZZ"))
		var status string
		_ = h.env.Pool.QueryRow(ctx, `SELECT status FROM provider_callbacks WHERE dedupe_key = 'ws_CO_unknown:0'`).Scan(&status)
		if status != "ignored" {
			t.Fatalf("status = %s", status)
		}
	})

	t.Run("expiry then late confirmed success", func(t *testing.T) {
		u := h.env.User(t, "Dep Eight")
		d := h.create(t, u, "100")
		if err := h.svc.Dispatch(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.env.Pool.Exec(ctx, `UPDATE deposits SET expires_at = now() - interval '1 minute' WHERE id = $1`, d.ID); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.Poll(ctx, d.ID); !isRetry(err) {
			t.Fatalf("poll in late window should reschedule: %v", err)
		}
		if d = get(t, h, d.ID); d.Status != "expired" || !d.NeedsReview {
			t.Fatalf("status %s review=%v", d.Status, d.NeedsReview)
		}
		h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomeSucceeded}, nil)
		defer h.prov.SetQuery(payments.StatusResult{Outcome: payments.OutcomePending}, nil)
		if err := h.svc.Poll(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if d = get(t, h, d.ID); d.Status != "succeeded" || !d.NeedsReview {
			t.Fatalf("late success: %s review=%v", d.Status, d.NeedsReview)
		}
	})

	t.Run("settle is single-posting even if called repeatedly", func(t *testing.T) {
		u := h.env.User(t, "Dep Nine")
		d := h.create(t, u, "100")
		_ = h.svc.Dispatch(ctx, d.ID)
		for i := 0; i < 3; i++ {
			if err := h.svc.Settle(ctx, d.ID, "", "0", "ok"); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		_ = h.env.Pool.QueryRow(ctx, `SELECT count(*) FROM journal_transactions WHERE reference_id = $1`, d.ID).Scan(&n)
		if n != 1 {
			t.Fatalf("journals = %d", n)
		}
		if bal, _ := ledger.Balance(ctx, h.env.Pool, u.Wallet.AvailableAccountID); bal != 10_000 {
			t.Fatalf("balance %d", bal)
		}
	})
}
