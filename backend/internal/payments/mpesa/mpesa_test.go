package mpesa

// These tests run against an httptest double that mimics the documented
// Daraja contract. They verify our request construction and response
// normalisation; they are NOT evidence of compatibility with the live API.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sovereigntrust/wallet/internal/config"
	"github.com/sovereigntrust/wallet/internal/payments"
)

func newTestClient(t *testing.T, h http.Handler) (*Client, *atomic.Int64) {
	t.Helper()
	var tokenCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/v1/generate", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "key" || p != "secret" || r.URL.Query().Get("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		tokenCalls.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"tok123","expires_in":"3599"}`))
	})
	mux.Handle("/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := New(config.MpesaConfig{Environment: "sandbox", BaseURL: srv.URL, ConsumerKey: "key", ConsumerSecret: "secret",
		ShortCode: "174379", PartyB: "174379", Passkey: "passkey", TransactionType: "CustomerPayBillOnline"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC) }
	return c, &tokenCalls
}

func TestSTKPushRequestShape(t *testing.T) {
	var got map[string]any
	c, tokens := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mpesa/stkpush/v1/processrequest" || r.Header.Get("Authorization") != "Bearer tok123" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"MerchantRequestID":"m-1","CheckoutRequestID":"ws_CO_1","ResponseCode":"0","ResponseDescription":"Success. Request accepted for processing","CustomerMessage":"ok"}`))
	}))
	resp, err := c.InitiateCollection(context.Background(), payments.CollectionRequest{
		Reference: "DEP0123456789XYZ", MSISDN: "+254712345678", Amount: 500000, Currency: "KES",
		Description: "Wallet deposit long description", CallbackURL: "https://example.com/cb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.CheckoutRef != "ws_CO_1" || resp.MerchantRef != "m-1" {
		t.Fatalf("resp = %+v", resp)
	}
	// 09:00 UTC = 12:00 EAT
	wantPw := base64.StdEncoding.EncodeToString([]byte("174379passkey20261010120000"))
	checks := map[string]any{
		"Timestamp": "20261010120000", "Password": wantPw, "Amount": float64(5000), "PartyA": "254712345678",
		"PhoneNumber": "254712345678", "PartyB": "174379", "AccountReference": "DEP012345678", "TransactionDesc": "Wallet deposi",
	}
	for k, v := range checks {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	// Token is cached across calls.
	_, _ = c.InitiateCollection(context.Background(), payments.CollectionRequest{Reference: "r2", MSISDN: "+254712345678", Amount: 100, Currency: "KES"})
	if tokens.Load() != 1 {
		t.Errorf("token fetched %d times, want 1", tokens.Load())
	}
}

func TestSTKPushRejectsFractionalShillings(t *testing.T) {
	c, _ := newTestClient(t, http.NotFoundHandler())
	_, err := c.InitiateCollection(context.Background(), payments.CollectionRequest{Reference: "r", MSISDN: "+254712345678", Amount: 150, Currency: "KES"})
	if !errors.Is(err, payments.ErrRejected) {
		t.Fatalf("got %v", err)
	}
}

func TestSTKPushErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{400, payments.ErrRejected},
		{429, payments.ErrNotSent},
		{500, payments.ErrUncertain},
		{503, payments.ErrUncertain},
	}
	for _, tc := range cases {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"requestId":"x","errorCode":"400.002.02","errorMessage":"Bad Request"}`))
		}))
		_, err := c.InitiateCollection(context.Background(), payments.CollectionRequest{Reference: "r", MSISDN: "+254712345678", Amount: 100, Currency: "KES"})
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: got %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestDialFailureIsNotSent(t *testing.T) {
	c, err := New(config.MpesaConfig{BaseURL: "http://127.0.0.1:1", ConsumerKey: "k", ConsumerSecret: "s"}, payments.NewHTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.InitiateCollection(context.Background(), payments.CollectionRequest{Reference: "r", MSISDN: "+254712345678", Amount: 100, Currency: "KES"})
	if !errors.Is(err, payments.ErrNotSent) {
		t.Fatalf("got %v, want ErrNotSent", err)
	}
}

func TestSTKQueryNormalisation(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   payments.Outcome
	}{
		{200, `{"ResponseCode":"0","ResultCode":"0","ResultDesc":"The service request is processed successfully."}`, payments.OutcomeSucceeded},
		{200, `{"ResponseCode":"0","ResultCode":"1032","ResultDesc":"Request cancelled by user"}`, payments.OutcomeFailed},
		{200, `{"ResponseCode":"0","ResultCode":1037,"ResultDesc":"DS timeout user cannot be reached"}`, payments.OutcomeFailed},
		{200, `{"ResponseCode":"0","ResultCode":"4999","ResultDesc":"still under processing"}`, payments.OutcomePending},
		{200, `{"ResponseCode":"0","ResultCode":"7777","ResultDesc":"something new"}`, payments.OutcomeUnknown},
		{500, `{"requestId":"x","errorCode":"500.001.1001","errorMessage":"The transaction is being processed"}`, payments.OutcomePending},
	}
	for _, tc := range cases {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		res, err := c.QueryCollection(context.Background(), payments.StatusQuery{CheckoutRef: "ws_CO_1"})
		if err != nil {
			t.Errorf("%s: %v", tc.body, err)
			continue
		}
		if res.Outcome != tc.want {
			t.Errorf("%s: outcome %s, want %s", tc.body, res.Outcome, tc.want)
		}
	}
}

func TestParseSTKCallback(t *testing.T) {
	raw := []byte(`{"Body":{"stkCallback":{"MerchantRequestID":"29115-34620561-1","CheckoutRequestID":"ws_CO_191220191020363925",
		"ResultCode":0,"ResultDesc":"The service request is processed successfully.","CallbackMetadata":{"Item":[
		{"Name":"Amount","Value":1.00},{"Name":"MpesaReceiptNumber","Value":"NLJ7RT61SV"},{"Name":"TransactionDate","Value":20191219102115},
		{"Name":"PhoneNumber","Value":254708374149}]}}}}`)
	cb, err := ParseSTKCallback(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cb.ResultCode != "0" || cb.Receipt != "NLJ7RT61SV" || cb.Amount != 100 || cb.PhoneNumber != "254708374149" {
		t.Fatalf("parsed %+v", cb)
	}
	failed, err := ParseSTKCallback([]byte(`{"Body":{"stkCallback":{"MerchantRequestID":"a","CheckoutRequestID":"b","ResultCode":1032,"ResultDesc":"Request cancelled by user"}}}`))
	if err != nil || failed.ResultCode != "1032" {
		t.Fatalf("failed callback: %+v %v", failed, err)
	}
	if _, err := ParseSTKCallback([]byte(`{"Body":{}}`)); err == nil {
		t.Fatal("accepted callback without CheckoutRequestID")
	}
}

func TestParseB2CResult(t *testing.T) {
	raw := []byte(`{"Result":{"ResultType":0,"ResultCode":0,"ResultDesc":"The service request is processed successfully.",
		"OriginatorConversationID":"orig-1","ConversationID":"AG_2019","TransactionID":"NLJ41HAY6Q",
		"ResultParameters":{"ResultParameter":[{"Key":"TransactionAmount","Value":10},{"Key":"TransactionReceipt","Value":"NLJ41HAY6Q"}]}}}`)
	r, err := ParseB2CResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.ConversationID != "AG_2019" || r.Amount != 1000 || NormalizeB2CResult(r).Outcome != payments.OutcomeSucceeded {
		t.Fatalf("parsed %+v", r)
	}
	fail, _ := ParseB2CResult([]byte(`{"Result":{"ResultType":0,"ResultCode":2001,"ResultDesc":"The initiator information is invalid.","OriginatorConversationID":"o","ConversationID":"c","TransactionID":"X"}}`))
	if NormalizeB2CResult(fail).Outcome != payments.OutcomeFailed {
		t.Fatal("failed B2C result not normalized as failed")
	}
}

func TestB2CQueryUnsupported(t *testing.T) {
	c, _ := newTestClient(t, http.NotFoundHandler())
	if c.Capabilities().PayoutStatusQuery {
		t.Fatal("Daraja B2C must not claim synchronous status query")
	}
	if _, err := c.QueryPayout(context.Background(), payments.StatusQuery{}); !errors.Is(err, payments.ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}
