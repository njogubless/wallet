package admin_test

import (
	"context"
	"testing"

	"github.com/sovereigntrust/wallet/internal/admin"
	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/testutil"
	"github.com/sovereigntrust/wallet/internal/testutil/fixture"
)

func TestFinancialRequestsMakerChecker(t *testing.T) {
	pool := testutil.NewDB(t)
	env := fixture.New(pool)
	svc := &admin.Service{Pool: pool}
	ctx := context.Background()
	maker, checker, customer := env.User(t, "Maker Admin"), env.User(t, "Checker Admin"), env.User(t, "Customer One")

	credit, err := svc.CreateFinancialRequest(ctx, maker.ID, admin.CreateRequestInput{
		Kind: "adjustment_credit", WalletID: &customer.Wallet.ID, Amount: "250.00", ReasonCode: "goodwill", Memo: "Compensation for outage on 10 Oct"})
	if err != nil {
		t.Fatal(err)
	}
	if b := env.Balance(t, customer.Wallet); b.Available != 0 {
		t.Fatal("money moved before approval")
	}
	if _, err := svc.Decide(ctx, maker.ID, credit.ID, true, "self approve"); !apperr.Is(err, apperr.CodeForbidden) {
		t.Fatalf("self-approval: %v", err)
	}
	f, err := svc.Decide(ctx, checker.ID, credit.ID, true, "verified outage report")
	if err != nil || f.Status != "posted" || f.JournalID == nil {
		t.Fatalf("approve: %+v %v", f, err)
	}
	if b := env.Balance(t, customer.Wallet); b.Available != 25_000 {
		t.Fatalf("after credit: %+v", b)
	}
	if _, err := svc.Decide(ctx, checker.ID, credit.ID, true, "again"); !apperr.Is(err, apperr.CodeInvalidState) {
		t.Fatalf("double decision: %v", err)
	}

	// A debit larger than the balance is recorded as failed, not partially applied.
	debit, err := svc.CreateFinancialRequest(ctx, maker.ID, admin.CreateRequestInput{
		Kind: "adjustment_debit", WalletID: &customer.Wallet.ID, Amount: "1000", ReasonCode: "operational_error", Memo: "Duplicate credit correction"})
	if err != nil {
		t.Fatal(err)
	}
	f, err = svc.Decide(ctx, checker.ID, debit.ID, true, "ok")
	if !apperr.Is(err, apperr.CodeInsufficientFunds) || f == nil || f.Status != "failed" {
		t.Fatalf("debit: %+v %v", f, err)
	}
	if b := env.Balance(t, customer.Wallet); b.Available != 25_000 {
		t.Fatalf("balance changed by failed debit: %+v", b)
	}

	// Bad memo / reason rejected.
	if _, err := svc.CreateFinancialRequest(ctx, maker.ID, admin.CreateRequestInput{
		Kind: "adjustment_credit", WalletID: &customer.Wallet.ID, Amount: "1", ReasonCode: "because", Memo: "x"}); err == nil {
		t.Fatal("invalid request accepted")
	}

	// Status restrictions are audited and block closing a funded account.
	if err := svc.SetUserStatus(ctx, maker.ID, customer.ID, "closed", "customer request"); !apperr.Is(err, apperr.CodeInvalidState) {
		t.Fatalf("close funded account: %v", err)
	}
	if err := svc.SetUserStatus(ctx, maker.ID, customer.ID, "restricted", "fraud review"); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'admin.%'`).Scan(&n)
	if n < 5 {
		t.Fatalf("audit events = %d", n)
	}
	env.AssertLedgerConsistent(t)
}
