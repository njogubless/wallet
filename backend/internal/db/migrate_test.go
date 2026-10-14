package db_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sovereigntrust/wallet/internal/db"
)

// TestMigrations exercises: upgrade of a database that already holds
// representative records, full rollback to empty, and re-apply on clean.
func TestMigrations(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := "wm_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	u, _ := url.Parse(base)
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := db.MigrateTo(ctx, pool, 4); err != nil {
		t.Fatalf("up to 4: %v", err)
	}
	// Representative records before the final migration.
	uid := uuid.New()
	_, err = pool.Exec(ctx, `
		WITH a AS (INSERT INTO ledger_accounts (id, kind, currency, normal_side) VALUES
		             (gen_random_uuid(), 'wallet_available', 'KES', 'credit'), (gen_random_uuid(), 'wallet_hold', 'KES', 'credit')
		           RETURNING id, kind),
		     u AS (INSERT INTO users (id, phone, full_name, password_hash, status) VALUES ($1, '+254700000001', 'Migr User', 'x', 'active') RETURNING id)
		INSERT INTO wallets (id, user_id, account_number, currency, available_account_id, hold_account_id)
		SELECT gen_random_uuid(), (SELECT id FROM u), '100000000008', 'KES',
		       (SELECT id FROM a WHERE kind = 'wallet_available'), (SELECT id FROM a WHERE kind = 'wallet_hold')`, uid)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO deposits (id, user_id, wallet_id, provider, provider_env, msisdn, amount, currency, status, expires_at)
		SELECT gen_random_uuid(), $1, w.id, 'mpesa', 'simulated', '+254700000001', 1000, 'KES', 'pending_provider', now()
		  FROM wallets w WHERE w.user_id = $1`, uid)
	if err != nil {
		t.Fatalf("seed deposit: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("upgrade with data: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM wallet_activity WHERE user_id = $1`, uid).Scan(&n); err != nil || n != 1 {
		t.Fatalf("activity view after upgrade: %d %v", n, err)
	}
	// Full rollback must succeed on a database with rows in non-ledger tables
	// once ledger tables are empty of posted entries.
	if _, err := pool.Exec(ctx, `DELETE FROM deposits; DELETE FROM wallets; DELETE FROM ledger_accounts WHERE code IS NULL; DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateDownTo(ctx, pool, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("re-apply on clean: %v", err)
	}
}
