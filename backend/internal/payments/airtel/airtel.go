// Package airtel adapts the Airtel Money Africa Open API (collections and
// disbursements).
//
// VERIFICATION STATUS: endpoint paths, headers and payloads follow Airtel's
// publicly described v1 contracts (OAuth2 token, /merchant/v1/payments/,
// /standard/v1/payments/{id}, /standard/v1/disbursements/), but were NOT
// re-verified against developers.airtel.africa while building this adapter.
// Newer API versions may require message signing/encryption headers; confirm
// the version enabled for your merchant app during onboarding.
//
// Security model: Airtel can optionally add a "hash" to callbacks
// ("callback authentication"). Its exact construction could not be verified,
// so this adapter does NOT rely on it. Every callback is a hint only; outcomes
// are taken from the authenticated Transaction Enquiry APIs.
package airtel

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sovereigntrust/wallet/internal/config"
	"github.com/sovereigntrust/wallet/internal/money"
	"github.com/sovereigntrust/wallet/internal/payments"
	"github.com/sovereigntrust/wallet/internal/phone"
)

type Client struct {
	cfg  config.AirtelConfig
	http *http.Client
	now  func() time.Time

	mu       sync.Mutex
	token    string
	tokenExp time.Time

	pinKey *rsa.PublicKey
}

func New(cfg config.AirtelConfig, hc *http.Client) (*Client, error) {
	c := &Client{cfg: cfg, http: hc, now: time.Now}
	if cfg.PublicKeyPEM != "" {
		k, err := parsePublicKey(cfg.PublicKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("AIRTEL_PUBLIC_KEY: %w", err)
		}
		c.pinKey = k
	}
	return c, nil
}

func parsePublicKey(s string) (*rsa.PublicKey, error) {
	var der []byte
	if block, _ := pem.Decode([]byte(s)); block != nil {
		der = block.Bytes
	} else {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
		if err != nil {
			return nil, errors.New("neither PEM nor base64 DER")
		}
		der = b
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("not an RSA public key")
	}
	return rk, nil
}

func (c *Client) Name() string        { return "airtel" }
func (c *Client) Environment() string { return c.cfg.Environment }

func (c *Client) Capabilities() payments.Capabilities {
	return payments.Capabilities{
		Collection: true, CollectionStatusQuery: true,
		Payout: c.pinKey != nil && c.cfg.DisbursementPIN != "", PayoutStatusQuery: true,
		SignedCallbacks: false, WholeUnitsOnly: true,
	}
}

type tokenResp struct {
	AccessToken string              `json:"access_token"`
	ExpiresIn   payments.FlexString `json:"expires_in"`
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.tokenExp) {
		return c.token, nil
	}
	var tr tokenResp
	status, _, err := payments.DoJSON(ctx, c.http, http.MethodPost, c.cfg.BaseURL+"/auth/oauth2/token", nil,
		map[string]string{"client_id": c.cfg.ClientID, "client_secret": c.cfg.ClientSecret, "grant_type": "client_credentials"}, &tr)
	if err != nil || status != http.StatusOK || tr.AccessToken == "" {
		return "", &payments.ProviderError{Class: payments.ErrNotSent, Message: "oauth failed", HTTPStatus: status}
	}
	ttl := 180 * time.Second
	var secs int
	if _, err := fmt.Sscanf(string(tr.ExpiresIn), "%d", &secs); err == nil && secs > 0 {
		ttl = time.Duration(secs) * time.Second
	}
	c.token = tr.AccessToken
	c.tokenExp = c.now().Add(ttl - min(30*time.Second, ttl/4))
	return c.token, nil
}

func (c *Client) headers(tok string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + tok,
		"X-Country":     c.cfg.Country,
		"X-Currency":    c.cfg.Currency,
	}
}

type statusBlock struct {
	Code         payments.FlexString `json:"code"`
	Message      string              `json:"message"`
	ResultCode   string              `json:"result_code"`
	ResponseCode string              `json:"response_code"`
	Success      bool                `json:"success"`
}

type txnResp struct {
	Data struct {
		Transaction struct {
			ID            string `json:"id"`
			AirtelMoneyID string `json:"airtel_money_id"`
			ReferenceID   string `json:"reference_id"`
			Status        string `json:"status"`
			Message       string `json:"message"`
		} `json:"transaction"`
	} `json:"data"`
	Status statusBlock `json:"status"`
}

func (c *Client) amount(amount int64, currency string) (int64, error) {
	if currency != c.cfg.Currency || currency != money.KES.Code {
		return 0, &payments.ProviderError{Class: payments.ErrRejected, Message: "unsupported currency for Airtel " + c.cfg.Country}
	}
	v, err := money.ToWholeMajor(amount, money.KES)
	if err != nil {
		return 0, &payments.ProviderError{Class: payments.ErrRejected, Message: "Airtel amounts must be whole shillings"}
	}
	return v, nil
}

func classify(status int, s statusBlock) error {
	switch {
	case status == http.StatusTooManyRequests:
		return &payments.ProviderError{Class: payments.ErrNotSent, Code: s.ResponseCode, Message: s.Message, HTTPStatus: status}
	case status >= 400 && status < 500:
		return &payments.ProviderError{Class: payments.ErrRejected, Code: s.ResponseCode, Message: s.Message, HTTPStatus: status}
	default:
		return &payments.ProviderError{Class: payments.ErrUncertain, Code: s.ResponseCode, Message: s.Message, HTTPStatus: status}
	}
}

// InitiateCollection sends a USSD push. transaction.id is our reference and
// must be unique per request; Airtel uses it for enquiry.
func (c *Client) InitiateCollection(ctx context.Context, req payments.CollectionRequest) (payments.CollectionResponse, error) {
	amt, err := c.amount(req.Amount, req.Currency)
	if err != nil {
		return payments.CollectionResponse{}, err
	}
	tok, err := c.accessToken(ctx)
	if err != nil {
		return payments.CollectionResponse{}, err
	}
	body := map[string]any{
		"reference": truncate(req.Description, 64),
		"subscriber": map[string]any{
			"country": c.cfg.Country, "currency": c.cfg.Currency, "msisdn": phone.National(req.MSISDN),
		},
		"transaction": map[string]any{
			"amount": amt, "country": c.cfg.Country, "currency": c.cfg.Currency, "id": req.Reference,
		},
	}
	var out txnResp
	status, _, err := payments.DoJSON(ctx, c.http, http.MethodPost, c.cfg.BaseURL+"/merchant/v1/payments/", c.headers(tok), body, &out)
	if err != nil {
		return payments.CollectionResponse{}, err
	}
	if status != http.StatusOK {
		return payments.CollectionResponse{}, classify(status, out.Status)
	}
	if !out.Status.Success {
		return payments.CollectionResponse{}, &payments.ProviderError{Class: payments.ErrRejected, Code: out.Status.ResponseCode, Message: out.Status.Message, HTTPStatus: status}
	}
	return payments.CollectionResponse{RequestRef: req.Reference, CheckoutRef: req.Reference,
		ResultCode: out.Status.ResponseCode, ResultDesc: out.Status.Message}, nil
}

// NormalizeStatus maps Airtel transaction status codes.
func NormalizeStatus(code, message, airtelMoneyID string) payments.StatusResult {
	r := payments.StatusResult{ResultCode: code, ResultDesc: message, Receipt: airtelMoneyID}
	switch strings.ToUpper(code) {
	case "TS":
		r.Outcome = payments.OutcomeSucceeded
	case "TF":
		r.Outcome = payments.OutcomeFailed
	case "TIP":
		r.Outcome = payments.OutcomePending
	default: // "TA" (ambiguous) and anything unrecognised
		r.Outcome = payments.OutcomeUnknown
	}
	return r
}

func (c *Client) enquire(ctx context.Context, path, id string) (payments.StatusResult, error) {
	if id == "" {
		return payments.StatusResult{Outcome: payments.OutcomeUnknown, ResultDesc: "no reference to query"}, nil
	}
	tok, err := c.accessToken(ctx)
	if err != nil {
		return payments.StatusResult{}, err
	}
	var out txnResp
	status, _, err := payments.DoJSON(ctx, c.http, http.MethodGet, c.cfg.BaseURL+path+url.PathEscape(id), c.headers(tok), nil, &out)
	if err != nil {
		return payments.StatusResult{}, err
	}
	if status != http.StatusOK {
		return payments.StatusResult{}, &payments.ProviderError{Class: payments.ErrUncertain, Code: out.Status.ResponseCode, Message: out.Status.Message, HTTPStatus: status}
	}
	t := out.Data.Transaction
	return NormalizeStatus(t.Status, t.Message, t.AirtelMoneyID), nil
}

func (c *Client) QueryCollection(ctx context.Context, q payments.StatusQuery) (payments.StatusResult, error) {
	return c.enquire(ctx, "/standard/v1/payments/", q.RequestRef)
}

// EncryptPIN RSA-encrypts the disbursement PIN with Airtel's public key.
func (c *Client) encryptPIN() (string, error) {
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, c.pinKey, []byte(c.cfg.DisbursementPIN))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (c *Client) InitiatePayout(ctx context.Context, req payments.PayoutRequest) (payments.PayoutResponse, error) {
	if !c.Capabilities().Payout {
		return payments.PayoutResponse{}, &payments.ProviderError{Class: payments.ErrNotSent, Message: "Airtel disbursement not configured"}
	}
	amt, err := c.amount(req.Amount, req.Currency)
	if err != nil {
		return payments.PayoutResponse{}, err
	}
	pin, err := c.encryptPIN()
	if err != nil {
		return payments.PayoutResponse{}, &payments.ProviderError{Class: payments.ErrNotSent, Message: "encrypt PIN: " + err.Error()}
	}
	tok, err := c.accessToken(ctx)
	if err != nil {
		return payments.PayoutResponse{}, err
	}
	body := map[string]any{
		"payee":       map[string]any{"msisdn": phone.National(req.MSISDN)},
		"reference":   truncate(req.Remarks, 64),
		"pin":         pin,
		"transaction": map[string]any{"amount": amt, "id": req.Reference},
	}
	var out txnResp
	status, _, err := payments.DoJSON(ctx, c.http, http.MethodPost, c.cfg.BaseURL+"/standard/v1/disbursements/", c.headers(tok), body, &out)
	if err != nil {
		return payments.PayoutResponse{}, err
	}
	if status != http.StatusOK {
		return payments.PayoutResponse{}, classify(status, out.Status)
	}
	if !out.Status.Success {
		// A 200 with success=false may still be ambiguous for disbursements;
		// only enquiry can settle it.
		return payments.PayoutResponse{}, &payments.ProviderError{Class: payments.ErrUncertain, Code: out.Status.ResponseCode, Message: out.Status.Message, HTTPStatus: status}
	}
	return payments.PayoutResponse{RequestRef: req.Reference, ConversationRef: out.Data.Transaction.ReferenceID,
		ResultCode: out.Status.ResponseCode, ResultDesc: out.Status.Message}, nil
}

func (c *Client) QueryPayout(ctx context.Context, q payments.StatusQuery) (payments.StatusResult, error) {
	return c.enquire(ctx, "/standard/v1/disbursements/", q.RequestRef)
}

// Callback is the (unauthenticated) Airtel callback body.
type Callback struct {
	ID            string
	StatusCode    string
	Message       string
	AirtelMoneyID string
}

func ParseCallback(raw []byte) (Callback, error) {
	var env struct {
		Transaction struct {
			ID            string `json:"id"`
			Message       string `json:"message"`
			StatusCode    string `json:"status_code"`
			AirtelMoneyID string `json:"airtel_money_id"`
		} `json:"transaction"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return Callback{}, err
	}
	if env.Transaction.ID == "" {
		return Callback{}, errors.New("missing transaction.id")
	}
	t := env.Transaction
	return Callback{ID: t.ID, StatusCode: t.StatusCode, Message: t.Message, AirtelMoneyID: t.AirtelMoneyID}, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
