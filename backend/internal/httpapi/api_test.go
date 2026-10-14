package httpapi_test

// End-to-end API tests: real HTTP handlers, real PostgreSQL, the outbox
// worker, and the deterministic payments SIMULATOR (no real provider).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sovereigntrust/wallet/internal/admin"
	"github.com/sovereigntrust/wallet/internal/auth"
	"github.com/sovereigntrust/wallet/internal/callbacks"
	"github.com/sovereigntrust/wallet/internal/config"
	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/httpapi"
	"github.com/sovereigntrust/wallet/internal/idempotency"
	"github.com/sovereigntrust/wallet/internal/outbox"
	"github.com/sovereigntrust/wallet/internal/payments"
	"github.com/sovereigntrust/wallet/internal/payments/simulator"
	"github.com/sovereigntrust/wallet/internal/ratelimit"
	"github.com/sovereigntrust/wallet/internal/reconciliation"
	"github.com/sovereigntrust/wallet/internal/statements"
	"github.com/sovereigntrust/wallet/internal/testutil"
	"github.com/sovereigntrust/wallet/internal/testutil/fixture"
	"github.com/sovereigntrust/wallet/internal/transfers"
	"github.com/sovereigntrust/wallet/internal/withdrawals"
)

type client struct {
	t     *testing.T
	base  string
	token string
}

func (c *client) do(method, path string, body any, headers ...string) (int, map[string]any, http.Header) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rdr = strings.NewReader(s)
		} else {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
	}
	req, _ := http.NewRequest(method, c.base+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Header
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		return fmt.Sprint(e["code"])
	}
	return ""
}

func TestAPIEndToEnd(t *testing.T) {
	pool := testutil.NewDB(t)
	env := fixture.New(pool)
	sim := simulator.New("mpesa")
	sim.SettleAfter = 0
	providers := payments.Registry{"mpesa": sim}
	cfg := &config.Config{Env: "test", PaymentsMode: "simulated", CallbackToken: "cb-secret-token-0123456789abcdef"}
	idem := &idempotency.Store{Pool: pool}
	dep := &deposits.Service{Pool: pool, Providers: providers, Idem: idem, Expiry: time.Minute, Log: env.Log}
	wd := &withdrawals.Service{Pool: pool, Providers: providers, Idem: idem, Auth: env.Auth, Log: env.Log}
	srv := &httpapi.Server{Cfg: cfg, Pool: pool, Log: env.Log, Auth: env.Auth, Tokens: env.Auth.Tokens,
		Transfers: &transfers.Service{Pool: pool, Auth: env.Auth, Idem: idem}, Deposits: dep, Withdrawals: wd,
		Callbacks: &callbacks.Store{Pool: pool}, Statements: &statements.Service{Pool: pool}, Admin: &admin.Service{Pool: pool},
		Reconciliation: &reconciliation.Service{Pool: pool, Log: env.Log}, Providers: providers, Limiter: ratelimit.NewMemory()}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &outbox.Runner{Pool: pool, Log: env.Log, WorkerID: "test", PollInterval: 100 * time.Millisecond,
		Handlers: map[string]outbox.Handler{
			deposits.JobDispatch: func(ctx context.Context, j outbox.Job) error { return dep.Dispatch(ctx, jobID(j, "deposit_id")) },
			deposits.JobPoll:     func(ctx context.Context, j outbox.Job) error { return dep.Poll(ctx, jobID(j, "deposit_id")) },
			withdrawals.JobDispatch: func(ctx context.Context, j outbox.Job) error {
				return wd.Dispatch(ctx, jobID(j, "withdrawal_id"))
			},
			withdrawals.JobPoll: func(ctx context.Context, j outbox.Job) error { return wd.Poll(ctx, jobID(j, "withdrawal_id")) },
		}}
	go func() { _ = runner.Run(ctx) }()

	register := func(name string) (*client, string) {
		c := &client{t: t, base: ts.URL}
		ph := fmt.Sprintf("07%08d", rand.IntN(100_000_000))
		st, body, _ := c.do("POST", "/api/v1/auth/register", map[string]string{"phone": ph, "full_name": name, "password": fixture.Password})
		if st != 201 {
			t.Fatalf("register %d %v", st, body)
		}
		c.token = body["access_token"].(string)
		st, body, _ = c.do("POST", "/api/v1/me/pin", map[string]string{"password": fixture.Password, "pin": fixture.PIN})
		if st != 204 {
			t.Fatalf("set pin %d %v", st, body)
		}
		return c, ph
	}

	alice, alicePhone := register("Alice Wanjiru")
	bob, bobPhone := register("Bob Otieno")

	t.Run("unauthenticated and malformed requests", func(t *testing.T) {
		anon := &client{t: t, base: ts.URL}
		if st, _, _ := anon.do("GET", "/api/v1/wallets", nil); st != 401 {
			t.Errorf("anon wallets = %d", st)
		}
		bad := &client{t: t, base: ts.URL, token: "garbage"}
		if st, _, _ := bad.do("GET", "/api/v1/wallets", nil); st != 401 {
			t.Errorf("bad token = %d", st)
		}
		st, body, _ := alice.do("POST", "/api/v1/deposits", map[string]any{"provider": "mpesa", "phone": alicePhone, "amount": "100"})
		if st != 400 || errCode(body) != "idempotency_key_required" {
			t.Errorf("missing idempotency key: %d %v", st, body)
		}
		st, body, _ = alice.do("POST", "/api/v1/deposits", `{"provider":"mpesa","phone":"`+alicePhone+`","amount":100.5}`, "Idempotency-Key", uuid.NewString())
		if st != 422 {
			t.Errorf("numeric amount accepted: %d %v", st, body)
		}
		st, _, _ = alice.do("POST", "/api/v1/deposits", map[string]any{"provider": "mpesa", "phone": alicePhone, "amount": "100", "balance": "1000000"}, "Idempotency-Key", uuid.NewString())
		if st != 422 {
			t.Errorf("unknown field accepted: %d", st)
		}
	})

	var aliceWallet string
	t.Run("deposit settles via worker and simulator", func(t *testing.T) {
		st, body, _ := alice.do("GET", "/api/v1/wallets", nil)
		if st != 200 {
			t.Fatal(st)
		}
		aliceWallet = body["data"].([]any)[0].(map[string]any)["id"].(string)
		key := uuid.NewString()
		st, body, _ = alice.do("POST", "/api/v1/deposits", map[string]any{"provider": "mpesa", "phone": alicePhone, "amount": "5000"}, "Idempotency-Key", key)
		if st != 202 || body["simulated"] != true {
			t.Fatalf("deposit %d %v", st, body)
		}
		id := body["id"].(string)
		st, body2, hdr := alice.do("POST", "/api/v1/deposits", map[string]any{"provider": "mpesa", "phone": alicePhone, "amount": "5000"}, "Idempotency-Key", key)
		if st != 200 || hdr.Get("Idempotent-Replayed") != "true" || body2["id"] != id {
			t.Fatalf("replay %d %v", st, body2)
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			_, d, _ := alice.do("GET", "/api/v1/deposits/"+id, nil)
			if d["status"] == "succeeded" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("deposit not settled: %v", d)
			}
			time.Sleep(200 * time.Millisecond)
		}
		_, bal, _ := alice.do("GET", "/api/v1/wallets/"+aliceWallet+"/balance", nil)
		if bal["available"].(map[string]any)["amount"] != "5000.00" {
			t.Fatalf("balance %v", bal)
		}
	})

	t.Run("object ownership enforced (IDOR)", func(t *testing.T) {
		if st, _, _ := bob.do("GET", "/api/v1/wallets/"+aliceWallet+"/balance", nil); st != 404 {
			t.Errorf("bob read alice balance: %d", st)
		}
		if st, _, _ := bob.do("GET", "/api/v1/wallets/"+aliceWallet+"/transactions", nil); st != 404 {
			t.Errorf("bob read alice transactions: %d", st)
		}
		_, list, _ := alice.do("GET", "/api/v1/transactions", nil)
		depID := list["data"].([]any)[0].(map[string]any)["id"].(string)
		if st, _, _ := bob.do("GET", "/api/v1/deposits/"+depID, nil); st != 404 {
			t.Errorf("bob read alice deposit: %d", st)
		}
		if st, _, _ := bob.do("GET", "/api/v1/transactions/"+depID, nil); st != 404 {
			t.Errorf("bob read alice receipt: %d", st)
		}
	})

	t.Run("transfer with fee quote, lookup and receipt", func(t *testing.T) {
		st, q, _ := alice.do("GET", "/api/v1/fees/quote?operation=transfer&amount=1200", nil)
		if st != 200 || q["total"].(map[string]any)["amount"] != "1200.00" {
			t.Fatalf("quote %d %v", st, q)
		}
		st, rec, _ := alice.do("GET", "/api/v1/recipients/lookup?phone="+bobPhone, nil)
		if st != 200 || rec["display_name"] != "Bob O." {
			t.Fatalf("lookup %d %v", st, rec)
		}
		if _, ok := rec["user_id"]; ok {
			t.Error("lookup leaks user id")
		}
		st, tr, _ := alice.do("POST", "/api/v1/transfers", map[string]any{"recipient_phone": bobPhone, "amount": "1200", "pin": fixture.PIN, "description": "rent share"}, "Idempotency-Key", uuid.NewString())
		if st != 201 || tr["status"] != "succeeded" {
			t.Fatalf("transfer %d %v", st, tr)
		}
		st, wrong, _ := alice.do("POST", "/api/v1/transfers", map[string]any{"recipient_phone": bobPhone, "amount": "1", "pin": "9137"}, "Idempotency-Key", uuid.NewString())
		if st != 401 || errCode(wrong) != "invalid_pin" {
			t.Fatalf("wrong pin %d %v", st, wrong)
		}
		_, bal, _ := bob.do("GET", "/api/v1/transactions?type=transfer", nil)
		rows := bal["data"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["direction"] != "credit" {
			t.Fatalf("bob history %v", bal)
		}
		st, receipt, _ := bob.do("GET", "/api/v1/transactions/"+tr["id"].(string), nil)
		if st != 200 || receipt["receipt_number"] == nil {
			t.Fatalf("receipt %d %v", st, receipt)
		}
	})

	t.Run("withdrawal holds then settles", func(t *testing.T) {
		st, w, _ := alice.do("POST", "/api/v1/withdrawals", map[string]any{"provider": "mpesa", "amount": "1000", "pin": fixture.PIN}, "Idempotency-Key", uuid.NewString())
		if st != 202 {
			t.Fatalf("withdrawal %d %v", st, w)
		}
		id := w["id"].(string)
		deadline := time.Now().Add(15 * time.Second)
		for {
			_, d, _ := alice.do("GET", "/api/v1/withdrawals/"+id, nil)
			if d["status"] == "succeeded" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("withdrawal not settled: %v", d)
			}
			time.Sleep(200 * time.Millisecond)
		}
		_, bal, _ := alice.do("GET", "/api/v1/wallets/"+aliceWallet+"/balance", nil)
		// 5000 - 1200 - (1000 + 20 fee) = 2780
		if bal["available"].(map[string]any)["amount"] != "2780.00" || bal["held"].(map[string]any)["amount"] != "0.00" {
			t.Fatalf("balance %v", bal)
		}
	})

	t.Run("statement generation and download", func(t *testing.T) {
		today := time.Now().Format("2006-01-02")
		st, s, _ := alice.do("POST", "/api/v1/statements", map[string]any{"from": today, "to": today, "format": "csv"})
		if st != 201 {
			t.Fatalf("statement %d %v", st, s)
		}
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/statements/"+s["id"].(string)+"/download", nil)
		req.Header.Set("Authorization", "Bearer "+alice.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(raw), "Closing available balance,2780.00") {
			t.Fatalf("download %d %s", resp.StatusCode, raw)
		}
		if st, _, _ := bob.do("GET", "/api/v1/statements/"+s["id"].(string)+"/download", nil); st != 404 {
			t.Errorf("bob downloaded alice statement: %d", st)
		}
		st, _, _ = alice.do("POST", "/api/v1/statements", map[string]any{"from": today, "to": today, "format": "pdf"})
		if st != 201 {
			t.Fatalf("pdf statement %d", st)
		}
	})

	t.Run("admin endpoints require staff role", func(t *testing.T) {
		if st, _, _ := alice.do("GET", "/api/v1/admin/dashboard", nil); st != 403 {
			t.Fatalf("user reached admin: %d", st)
		}
		if _, err := pool.Exec(context.Background(), `UPDATE users SET role = 'support' WHERE phone = $1`, "+254"+alicePhone[1:]); err != nil {
			t.Fatal(err)
		}
		// Role changes apply immediately (checked per request).
		st, d, _ := alice.do("GET", "/api/v1/admin/dashboard", nil)
		if st != 200 || d["ledger_consistent"] != true {
			t.Fatalf("dashboard %d %v", st, d)
		}
		if st, _, _ := alice.do("POST", "/api/v1/admin/reconciliation/runs", nil); st != 403 {
			t.Fatalf("support ran admin-only action: %d", st)
		}
	})

	t.Run("callbacks require secret path token", func(t *testing.T) {
		anon := &client{t: t, base: ts.URL}
		body := `{"Body":{"stkCallback":{"MerchantRequestID":"m","CheckoutRequestID":"ws_x","ResultCode":0,"ResultDesc":"ok"}}}`
		if st, _, _ := anon.do("POST", "/api/v1/callbacks/mpesa/wrong-token/stk", body); st != 404 {
			t.Errorf("wrong token accepted: %d", st)
		}
		st, resp, _ := anon.do("POST", "/api/v1/callbacks/mpesa/"+cfg.CallbackToken+"/stk", body)
		if st != 200 || resp["ResultCode"] != float64(0) {
			t.Errorf("callback %d %v", st, resp)
		}
		if st, _, _ := anon.do("POST", "/api/v1/callbacks/mpesa/"+cfg.CallbackToken+"/stk", `{"nope":1}`); st != 422 {
			t.Errorf("malformed callback: %d", st)
		}
	})

	t.Run("refresh rotation and reuse detection", func(t *testing.T) {
		c := &client{t: t, base: ts.URL}
		_, sess, _ := c.do("POST", "/api/v1/auth/login", map[string]string{"phone": bobPhone, "password": fixture.Password})
		rt := sess["refresh_token"].(string)
		st, s2, _ := c.do("POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": rt})
		if st != 200 {
			t.Fatalf("refresh %d", st)
		}
		if st, _, _ := c.do("POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": rt}); st != 401 {
			t.Fatalf("reused refresh token accepted: %d", st)
		}
		// Reuse revoked the whole family, including the newest token.
		if st, _, _ := c.do("POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": s2["refresh_token"].(string)}); st != 401 {
			t.Fatalf("family not revoked: %d", st)
		}
		if st, _, _ := c.do("POST", "/api/v1/auth/login", map[string]string{"phone": bobPhone, "password": "wrong-password-x"}); st != 401 {
			t.Fatalf("bad password: %d", st)
		}
	})

	env.AssertLedgerConsistent(t)
	_ = auth.LogSMS{}
}

func jobID(j outbox.Job, field string) uuid.UUID {
	var p map[string]string
	_ = json.Unmarshal(j.Payload, &p)
	id, _ := uuid.Parse(p[field])
	return id
}
