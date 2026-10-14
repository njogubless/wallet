package reconciliation_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sovereigntrust/wallet/internal/reconciliation"
	"github.com/sovereigntrust/wallet/internal/testutil"
	"github.com/sovereigntrust/wallet/internal/testutil/fixture"
)

func TestParseStatementCSV(t *testing.T) {
	good := "provider,receipt,reference,direction,amount,currency,status,occurred_at\n" +
		"mpesa,QHK9482L,,in,5000.00,KES,completed,2026-10-10T09:15:00+03:00\n"
	rows, err := reconciliation.ParseStatementCSV(strings.NewReader(good), 10)
	if err != nil || len(rows) != 1 || rows[0].Amount != 500_000 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for _, bad := range []string{
		"wrong,header\n",
		"provider,receipt,reference,direction,amount,currency,status,occurred_at\nmpesa,,,in,1,KES,completed,2026-10-10T09:15:00Z\n",
		"provider,receipt,reference,direction,amount,currency,status,occurred_at\nmpesa,R,,sideways,1,KES,completed,2026-10-10T09:15:00Z\n",
		"provider,receipt,reference,direction,amount,currency,status,occurred_at\nmpesa,R,,in,1.001,KES,completed,2026-10-10T09:15:00Z\n",
		"provider,receipt,reference,direction,amount,currency,status,occurred_at\nmpesa,R,,in,1,KES,completed,yesterday\n",
	} {
		if _, err := reconciliation.ParseStatementCSV(strings.NewReader(bad), 10); err == nil {
			t.Errorf("accepted bad statement %q", bad)
		}
	}
}

func TestReconciliationIntegration(t *testing.T) {
	pool := testutil.NewDB(t)
	env := fixture.New(pool)
	svc := &reconciliation.Service{Pool: pool, Log: env.Log}
	ctx := context.Background()
	u := env.User(t, "Recon User")

	// A succeeded deposit with receipt R1, a pending one with request ref REFPEND,
	// and a succeeded one (R3) absent from the statement.
	mk := func(status, receipt, ref string, amount int64) {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO deposits (id, user_id, wallet_id, provider, provider_env, msisdn, amount, currency, status,
			                      provider_request_ref, provider_receipt, expires_at, completed_at)
			VALUES (gen_random_uuid(), $1, $2, 'mpesa', 'simulated', $3, $4, 'KES', 'pending_provider', NULLIF($5,''), NULLIF($6,''), now() + interval '1 hour',
			        CASE WHEN $7 = 'succeeded' THEN now() - interval '10 minutes' END)`,
			u.ID, u.Wallet.ID, u.Phone, amount, ref, receipt, status)
		if err != nil {
			t.Fatal(err)
		}
	}
	mk("pending_provider", "", "REFPEND", 100_000)
	mk("pending_provider", "R1", "", 200_000)

	stmt := []reconciliation.StatementRow{
		{Provider: "mpesa", Receipt: "R1", Direction: "in", Amount: 250_000, Currency: "KES", Status: "completed", OccurredAt: time.Now().Add(-30 * time.Minute)},
		{Provider: "mpesa", Receipt: "RPEND", Reference: "REFPEND", Direction: "in", Amount: 100_000, Currency: "KES", Status: "completed", OccurredAt: time.Now().Add(-20 * time.Minute)},
		{Provider: "mpesa", Receipt: "RGHOST", Direction: "in", Amount: 5_000, Currency: "KES", Status: "completed", OccurredAt: time.Now()},
		{Provider: "mpesa", Receipt: "RGHOST", Direction: "in", Amount: 5_000, Currency: "KES", Status: "completed", OccurredAt: time.Now()},
	}
	run, err := svc.ImportStatement(ctx, "mpesa", stmt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" {
		t.Fatalf("run %+v", run)
	}
	items, err := svc.ListItems(ctx, "open", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, it := range items {
		got[it.Discrepancy]++
	}
	want := map[string]int{"amount_mismatch": 1, "pending_but_completed": 1, "missing_internal": 1, "duplicate_provider_ref": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %d want %d (all: %v)", k, got[k], v, got)
		}
	}
	// Re-running does not duplicate open items.
	if _, err := svc.ImportStatement(ctx, "mpesa", stmt, nil); err != nil {
		t.Fatal(err)
	}
	again, _ := svc.ListItems(ctx, "open", "", 100)
	if len(again) != len(items) {
		t.Fatalf("items duplicated across runs: %d -> %d", len(items), len(again))
	}
	// Balances untouched.
	if b := env.Balance(t, u.Wallet); b.Available != 0 {
		t.Fatalf("reconciliation moved money: %+v", b)
	}
	// Internal run on a consistent ledger finds no drift.
	if _, err := svc.RunInternal(ctx, nil); err != nil {
		t.Fatal(err)
	}
	drift, _ := svc.ListItems(ctx, "open", "projection_drift", 10)
	if len(drift) != 0 {
		t.Fatalf("unexpected drift items: %d", len(drift))
	}
	// Resolution requires a note and is audited.
	if _, err := svc.UpdateItem(ctx, items[0].ID, "resolved", "short", u.ID); err == nil {
		t.Fatal("resolved without adequate note")
	}
	if _, err := svc.UpdateItem(ctx, items[0].ID, "resolved", "Confirmed with provider support ticket #123", u.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.CollectGauges(ctx); err != nil {
		t.Fatal(err)
	}
}
