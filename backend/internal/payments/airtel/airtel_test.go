package airtel

// Tests against an httptest double of the documented Airtel contract; not
// evidence of compatibility with the live API.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sovereigntrust/wallet/internal/config"
	"github.com/sovereigntrust/wallet/internal/payments"
)

func testServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_id"] != "id" || body["grant_type"] != "client_credentials" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"atok","expires_in":"180","token_type":"bearer"}`))
	})
	mux.HandleFunc("/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func cfg(url string) config.AirtelConfig {
	return config.AirtelConfig{Environment: "sandbox", BaseURL: url, ClientID: "id", ClientSecret: "secret", Country: "KE", Currency: "KES"}
}

func TestCollectionRequestShape(t *testing.T) {
	var body map[string]any
	var hdr http.Header
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/merchant/v1/payments/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hdr = r.Header
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"data":{"transaction":{"id":"DEP1","status":"Success."}},"status":{"code":"200","message":"Success.","result_code":"ESB000010","response_code":"DP00800001006","success":true}}`))
	})
	c, _ := New(cfg(srv.URL), srv.Client())
	resp, err := c.InitiateCollection(context.Background(), payments.CollectionRequest{Reference: "DEP1", MSISDN: "+254733123456", Amount: 100000, Currency: "KES", Description: "Wallet deposit"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.CheckoutRef != "DEP1" {
		t.Fatalf("resp %+v", resp)
	}
	if hdr.Get("X-Country") != "KE" || hdr.Get("X-Currency") != "KES" || hdr.Get("Authorization") != "Bearer atok" {
		t.Errorf("headers %v", hdr)
	}
	sub := body["subscriber"].(map[string]any)
	txn := body["transaction"].(map[string]any)
	if sub["msisdn"] != "733123456" || txn["amount"] != float64(1000) || txn["id"] != "DEP1" {
		t.Errorf("body %v", body)
	}
}

func TestEnquiryNormalisation(t *testing.T) {
	for code, want := range map[string]payments.Outcome{
		"TS": payments.OutcomeSucceeded, "TF": payments.OutcomeFailed, "TIP": payments.OutcomePending,
		"TA": payments.OutcomeUnknown, "XYZ": payments.OutcomeUnknown,
	} {
		srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/standard/v1/payments/DEP1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"transaction":{"airtel_money_id":"C3500000001","id":"DEP1","message":"m","status":"` + code + `"}},"status":{"code":"200","success":true}}`))
		})
		c, _ := New(cfg(srv.URL), srv.Client())
		res, err := c.QueryCollection(context.Background(), payments.StatusQuery{RequestRef: "DEP1"})
		if err != nil || res.Outcome != want {
			t.Errorf("%s: %v %v, want %v", code, res.Outcome, err, want)
		}
		if code == "TS" && res.Receipt != "C3500000001" {
			t.Errorf("receipt = %q", res.Receipt)
		}
	}
}

func TestDisbursementNeedsKeyAndPIN(t *testing.T) {
	c, _ := New(cfg("http://unused"), http.DefaultClient)
	if c.Capabilities().Payout {
		t.Fatal("payout capability without PIN/key")
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	var gotPIN string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotPIN, _ = body["pin"].(string)
		_, _ = w.Write([]byte(`{"data":{"transaction":{"reference_id":"R1","airtel_money_id":"A1","id":"WD1","status":"TS"}},"status":{"success":true,"response_code":"DP00900001001"}}`))
	})
	conf := cfg(srv.URL)
	conf.PublicKeyPEM, conf.DisbursementPIN = pemStr, "1234"
	c, err := New(conf, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.InitiatePayout(context.Background(), payments.PayoutRequest{Reference: "WD1", MSISDN: "+254733123456", Amount: 5000, Currency: "KES"}); err != nil {
		t.Fatal(err)
	}
	if gotPIN == "" || gotPIN == "1234" {
		t.Fatalf("PIN not encrypted: %q", gotPIN)
	}
}

func TestServerErrorIsUncertain(t *testing.T) {
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) })
	c, _ := New(cfg(srv.URL), srv.Client())
	_, err := c.InitiateCollection(context.Background(), payments.CollectionRequest{Reference: "D", MSISDN: "+254733123456", Amount: 100, Currency: "KES"})
	if !errors.Is(err, payments.ErrUncertain) {
		t.Fatalf("got %v", err)
	}
}

func TestParseCallback(t *testing.T) {
	cb, err := ParseCallback([]byte(`{"transaction":{"id":"DEP1","message":"Paid","status_code":"TS","airtel_money_id":"MP1"}}`))
	if err != nil || cb.ID != "DEP1" || cb.StatusCode != "TS" {
		t.Fatalf("%+v %v", cb, err)
	}
	if _, err := ParseCallback([]byte(`{"transaction":{}}`)); err == nil {
		t.Fatal("accepted callback without id")
	}
}
