// Package mpesa adapts Safaricom's M-Pesa Daraja API.
//
// VERIFICATION STATUS: request/response shapes follow Daraja's publicly
// documented contracts for OAuth, M-Pesa Express (STK Push + Query) and B2C,
// but were NOT re-verified against the official portal while building this
// adapter (the portal could not be fetched). Validate every field against
// developer.safaricom.co.ke during merchant onboarding and run the sandbox
// test plan in docs/providers/mpesa.md before enabling production.
//
// Security model: Daraja callbacks are not cryptographically signed. This
// adapter therefore treats callbacks as hints. Deposits are only credited
// after an authenticated STK Push Query confirms success. B2C has no
// synchronous status query, so B2C results are accepted only when they arrive
// on the secret callback path AND match the ConversationID and
// OriginatorConversationID Safaricom returned to us (plus the amount), with
// an optional source-IP allowlist. Anything inconsistent goes to review.
package mpesa

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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sovereigntrust/wallet/internal/config"
	"github.com/sovereigntrust/wallet/internal/money"
	"github.com/sovereigntrust/wallet/internal/payments"
	"github.com/sovereigntrust/wallet/internal/phone"
)

// Nairobi time for the Timestamp field (YYYYMMDDHHmmss).
var eat = time.FixedZone("EAT", 3*60*60)

type Client struct {
	cfg  config.MpesaConfig
	http *http.Client
	now  func() time.Time

	mu       sync.Mutex
	token    string
	tokenExp time.Time

	securityCredential string
	b2cPath            string
}

func New(cfg config.MpesaConfig, hc *http.Client) (*Client, error) {
	c := &Client{cfg: cfg, http: hc, now: time.Now, b2cPath: "/mpesa/b2c/v3/paymentrequest"}
	if p := os.Getenv("MPESA_B2C_PATH"); p != "" {
		c.b2cPath = p
	}
	if cfg.InitiatorName != "" && cfg.InitiatorPassword != "" && cfg.CertificatePath != "" {
		pemBytes, err := os.ReadFile(cfg.CertificatePath)
		if err != nil {
			return nil, fmt.Errorf("read MPESA_CERT_PATH: %w", err)
		}
		sc, err := SecurityCredential(cfg.InitiatorPassword, pemBytes)
		if err != nil {
			return nil, err
		}
		c.securityCredential = sc
	}
	return c, nil
}

func (c *Client) Name() string        { return "mpesa" }
func (c *Client) Environment() string { return c.cfg.Environment }

func (c *Client) Capabilities() payments.Capabilities {
	return payments.Capabilities{
		Collection:            true,
		CollectionStatusQuery: true,
		Payout:                c.securityCredential != "" && c.cfg.B2CShortCode != "",
		PayoutStatusQuery:     false,
		SignedCallbacks:       false,
		WholeUnitsOnly:        true,
	}
}

// SecurityCredential encrypts the initiator password with Safaricom's public
// certificate (RSA PKCS#1 v1.5) and base64-encodes it.
func SecurityCredential(password string, certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("mpesa certificate: no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("mpesa certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("mpesa certificate: not an RSA key")
	}
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(password))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ct), nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/oauth/v1/generate?grant_type=client_credentials", nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.cfg.ConsumerKey, c.cfg.ConsumerSecret)
	resp, err := c.http.Do(req)
	if err != nil {
		// Failing to get a token means the payment request was never sent.
		return "", &payments.ProviderError{Class: payments.ErrNotSent, Message: "oauth: " + err.Error()}
	}
	defer resp.Body.Close()
	var tr tokenResp
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&tr) != nil || tr.AccessToken == "" {
		return "", &payments.ProviderError{Class: payments.ErrNotSent, Message: "oauth failed", HTTPStatus: resp.StatusCode}
	}
	ttl := 3599 * time.Second
	var secs int
	if _, err := fmt.Sscanf(string(tr.ExpiresIn), "%d", &secs); err == nil && secs > 0 {
		ttl = time.Duration(secs) * time.Second
	}
	c.token = tr.AccessToken
	c.tokenExp = c.now().Add(ttl - 60*time.Second)
	return c.token, nil
}

func (c *Client) invalidateToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

func (c *Client) timestampPassword() (ts, pw string) {
	ts = c.now().In(eat).Format("20060102150405")
	pw = base64.StdEncoding.EncodeToString([]byte(c.cfg.ShortCode + c.cfg.Passkey + ts))
	return
}

type apiError struct {
	RequestID    string `json:"requestId"`
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}

type stkPushResp struct {
	apiError
	MerchantRequestID   string              `json:"MerchantRequestID"`
	CheckoutRequestID   string              `json:"CheckoutRequestID"`
	ResponseCode        payments.FlexString `json:"ResponseCode"`
	ResponseDescription string              `json:"ResponseDescription"`
	CustomerMessage     string              `json:"CustomerMessage"`
}

func (c *Client) post(ctx context.Context, path string, body, out any) (int, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return 0, err
	}
	status, _, err := payments.DoJSON(ctx, c.http, http.MethodPost, c.cfg.BaseURL+path,
		map[string]string{"Authorization": "Bearer " + tok}, body, out)
	if status == http.StatusUnauthorized {
		c.invalidateToken()
	}
	return status, err
}

func wholeShillings(amount int64, currency string) (int64, error) {
	if currency != "KES" {
		return 0, &payments.ProviderError{Class: payments.ErrRejected, Message: "M-Pesa supports KES only"}
	}
	v, err := money.ToWholeMajor(amount, money.KES)
	if err != nil {
		return 0, &payments.ProviderError{Class: payments.ErrRejected, Message: "M-Pesa amounts must be whole shillings"}
	}
	return v, nil
}

// classifyHTTP maps a non-success HTTP response for a money-moving call.
func classifyHTTP(status int, e apiError) error {
	switch {
	case status == http.StatusTooManyRequests:
		return &payments.ProviderError{Class: payments.ErrNotSent, Code: e.ErrorCode, Message: e.ErrorMessage, HTTPStatus: status}
	case status >= 400 && status < 500:
		return &payments.ProviderError{Class: payments.ErrRejected, Code: e.ErrorCode, Message: e.ErrorMessage, HTTPStatus: status}
	default:
		return &payments.ProviderError{Class: payments.ErrUncertain, Code: e.ErrorCode, Message: e.ErrorMessage, HTTPStatus: status}
	}
}

// InitiateCollection sends an M-Pesa Express (STK Push) prompt.
func (c *Client) InitiateCollection(ctx context.Context, req payments.CollectionRequest) (payments.CollectionResponse, error) {
	amt, err := wholeShillings(req.Amount, req.Currency)
	if err != nil {
		return payments.CollectionResponse{}, err
	}
	ts, pw := c.timestampPassword()
	msisdn := phone.MSISDN(req.MSISDN)
	accountRef := req.Reference
	if len(accountRef) > 12 { // Daraja AccountReference max length
		accountRef = accountRef[:12]
	}
	desc := req.Description
	if len(desc) > 13 { // TransactionDesc max length
		desc = desc[:13]
	}
	body := map[string]any{
		"BusinessShortCode": c.cfg.ShortCode,
		"Password":          pw,
		"Timestamp":         ts,
		"TransactionType":   c.cfg.TransactionType,
		"Amount":            amt,
		"PartyA":            msisdn,
		"PartyB":            c.cfg.PartyB,
		"PhoneNumber":       msisdn,
		"CallBackURL":       req.CallbackURL,
		"AccountReference":  accountRef,
		"TransactionDesc":   desc,
	}
	var out stkPushResp
	status, err := c.post(ctx, "/mpesa/stkpush/v1/processrequest", body, &out)
	if err != nil {
		return payments.CollectionResponse{}, err
	}
	if status != http.StatusOK || out.ResponseCode != "0" || out.CheckoutRequestID == "" {
		if status == http.StatusOK {
			return payments.CollectionResponse{}, &payments.ProviderError{Class: payments.ErrRejected, Code: string(out.ResponseCode), Message: out.ResponseDescription, HTTPStatus: status}
		}
		return payments.CollectionResponse{}, classifyHTTP(status, out.apiError)
	}
	return payments.CollectionResponse{
		RequestRef: req.Reference, CheckoutRef: out.CheckoutRequestID, MerchantRef: out.MerchantRequestID,
		ResultCode: string(out.ResponseCode), ResultDesc: out.ResponseDescription,
	}, nil
}

type stkQueryResp struct {
	apiError
	ResponseCode        payments.FlexString `json:"ResponseCode"`
	ResponseDescription string              `json:"ResponseDescription"`
	MerchantRequestID   string              `json:"MerchantRequestID"`
	CheckoutRequestID   string              `json:"CheckoutRequestID"`
	ResultCode          payments.FlexString `json:"ResultCode"`
	ResultDesc          string              `json:"ResultDesc"`
}

// STK result codes with a definitive "no money moved" meaning.
var stkFailureCodes = map[string]string{
	"1":    "insufficient M-Pesa balance",
	"1001": "subscriber busy / transaction in progress",
	"1019": "transaction expired",
	"1025": "error sending push request",
	"1032": "cancelled by customer",
	"1037": "customer could not be reached",
	"2001": "wrong M-Pesa PIN entered",
	"9999": "error sending push request",
}

// NormalizeSTKResult maps an STK ResultCode to a normalized outcome.
func NormalizeSTKResult(code, desc string) payments.StatusResult {
	r := payments.StatusResult{ResultCode: code, ResultDesc: desc}
	switch {
	case code == "0":
		r.Outcome = payments.OutcomeSucceeded
	case code == "4999": // still under processing
		r.Outcome = payments.OutcomePending
	case stkFailureCodes[code] != "":
		r.Outcome = payments.OutcomeFailed
		if r.ResultDesc == "" {
			r.ResultDesc = stkFailureCodes[code]
		}
	default:
		// Unknown codes are not assumed to be failures; they go to review.
		r.Outcome = payments.OutcomeUnknown
	}
	return r
}

// QueryCollection asks Daraja for the STK Push status. This call is
// authenticated with our OAuth token and is the trusted source of outcome.
// It does not return the M-Pesa receipt number.
func (c *Client) QueryCollection(ctx context.Context, q payments.StatusQuery) (payments.StatusResult, error) {
	if q.CheckoutRef == "" {
		return payments.StatusResult{Outcome: payments.OutcomeUnknown, ResultDesc: "no CheckoutRequestID to query"}, nil
	}
	ts, pw := c.timestampPassword()
	var out stkQueryResp
	status, err := c.post(ctx, "/mpesa/stkpushquery/v1/query", map[string]any{
		"BusinessShortCode": c.cfg.ShortCode, "Password": pw, "Timestamp": ts, "CheckoutRequestID": q.CheckoutRef,
	}, &out)
	if err != nil {
		return payments.StatusResult{}, err
	}
	if status != http.StatusOK {
		// Daraja answers "The transaction is being processed" with an error
		// payload while the customer has not yet responded.
		if out.ErrorCode == "500.001.1001" || strings.Contains(strings.ToLower(out.ErrorMessage), "being processed") {
			return payments.StatusResult{Outcome: payments.OutcomePending, ResultCode: out.ErrorCode, ResultDesc: out.ErrorMessage}, nil
		}
		return payments.StatusResult{}, &payments.ProviderError{Class: payments.ErrUncertain, Code: out.ErrorCode, Message: out.ErrorMessage, HTTPStatus: status}
	}
	if out.ResultCode == "" {
		return payments.StatusResult{Outcome: payments.OutcomePending, ResultDesc: out.ResponseDescription}, nil
	}
	return NormalizeSTKResult(string(out.ResultCode), out.ResultDesc), nil
}

type b2cResp struct {
	apiError
	ConversationID           string              `json:"ConversationID"`
	OriginatorConversationID string              `json:"OriginatorConversationID"`
	ResponseCode             payments.FlexString `json:"ResponseCode"`
	ResponseDescription      string              `json:"ResponseDescription"`
}

// InitiatePayout sends a B2C payment request. The final outcome arrives
// asynchronously on ResultURL (or QueueTimeOutURL).
func (c *Client) InitiatePayout(ctx context.Context, req payments.PayoutRequest) (payments.PayoutResponse, error) {
	if !c.Capabilities().Payout {
		return payments.PayoutResponse{}, &payments.ProviderError{Class: payments.ErrNotSent, Message: "B2C not configured"}
	}
	amt, err := wholeShillings(req.Amount, req.Currency)
	if err != nil {
		return payments.PayoutResponse{}, err
	}
	remarks := req.Remarks
	if remarks == "" {
		remarks = "Wallet withdrawal"
	}
	if len(remarks) > 100 {
		remarks = remarks[:100]
	}
	body := map[string]any{
		"OriginatorConversationID": req.Reference,
		"InitiatorName":            c.cfg.InitiatorName,
		"SecurityCredential":       c.securityCredential,
		"CommandID":                c.cfg.B2CCommandID,
		"Amount":                   amt,
		"PartyA":                   c.cfg.B2CShortCode,
		"PartyB":                   phone.MSISDN(req.MSISDN),
		"Remarks":                  remarks,
		"QueueTimeOutURL":          req.TimeoutURL,
		"ResultURL":                req.ResultURL,
		"Occasion":                 "",
	}
	var out b2cResp
	status, err := c.post(ctx, c.b2cPath, body, &out)
	if err != nil {
		return payments.PayoutResponse{}, err
	}
	if status != http.StatusOK || out.ResponseCode != "0" || out.ConversationID == "" {
		if status == http.StatusOK {
			return payments.PayoutResponse{}, &payments.ProviderError{Class: payments.ErrRejected, Code: string(out.ResponseCode), Message: out.ResponseDescription, HTTPStatus: status}
		}
		return payments.PayoutResponse{}, classifyHTTP(status, out.apiError)
	}
	ref := out.OriginatorConversationID
	if ref == "" {
		ref = req.Reference
	}
	return payments.PayoutResponse{RequestRef: ref, ConversationRef: out.ConversationID,
		ResultCode: string(out.ResponseCode), ResultDesc: out.ResponseDescription}, nil
}

// QueryPayout is unsupported: Daraja's Transaction Status API is asynchronous
// and returns its answer through another unsigned callback.
func (c *Client) QueryPayout(context.Context, payments.StatusQuery) (payments.StatusResult, error) {
	return payments.StatusResult{}, payments.ErrUnsupported
}

// ---- Callback parsing ----

type STKCallback struct {
	MerchantRequestID string
	CheckoutRequestID string
	ResultCode        string
	ResultDesc        string
	Amount            int64 // minor units, 0 if absent
	Receipt           string
	PhoneNumber       string
}

type stkEnvelope struct {
	Body struct {
		StkCallback struct {
			MerchantRequestID string              `json:"MerchantRequestID"`
			CheckoutRequestID string              `json:"CheckoutRequestID"`
			ResultCode        payments.FlexString `json:"ResultCode"`
			ResultDesc        string              `json:"ResultDesc"`
			CallbackMetadata  struct {
				Item []struct {
					Name  string          `json:"Name"`
					Value json.RawMessage `json:"Value"`
				} `json:"Item"`
			} `json:"CallbackMetadata"`
		} `json:"stkCallback"`
	} `json:"Body"`
}

func ParseSTKCallback(raw []byte) (STKCallback, error) {
	var env stkEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return STKCallback{}, err
	}
	cb := env.Body.StkCallback
	if cb.CheckoutRequestID == "" {
		return STKCallback{}, errors.New("missing CheckoutRequestID")
	}
	out := STKCallback{MerchantRequestID: cb.MerchantRequestID, CheckoutRequestID: cb.CheckoutRequestID,
		ResultCode: string(cb.ResultCode), ResultDesc: cb.ResultDesc}
	for _, it := range cb.CallbackMetadata.Item {
		var v payments.FlexString
		_ = json.Unmarshal(it.Value, &v)
		switch it.Name {
		case "Amount":
			out.Amount = parseMajorToMinor(string(v))
		case "MpesaReceiptNumber":
			out.Receipt = string(v)
		case "PhoneNumber":
			out.PhoneNumber = string(v)
		}
	}
	return out, nil
}

type B2CResult struct {
	ResultType               string
	ResultCode               string
	ResultDesc               string
	OriginatorConversationID string
	ConversationID           string
	TransactionID            string
	Amount                   int64
}

type b2cEnvelope struct {
	Result struct {
		ResultType               payments.FlexString `json:"ResultType"`
		ResultCode               payments.FlexString `json:"ResultCode"`
		ResultDesc               string              `json:"ResultDesc"`
		OriginatorConversationID string              `json:"OriginatorConversationID"`
		ConversationID           string              `json:"ConversationID"`
		TransactionID            string              `json:"TransactionID"`
		ResultParameters         struct {
			ResultParameter json.RawMessage `json:"ResultParameter"`
		} `json:"ResultParameters"`
	} `json:"Result"`
}

type kv struct {
	Key   string              `json:"Key"`
	Value payments.FlexString `json:"Value"`
}

func ParseB2CResult(raw []byte) (B2CResult, error) {
	var env b2cEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return B2CResult{}, err
	}
	r := env.Result
	if r.ConversationID == "" && r.OriginatorConversationID == "" {
		return B2CResult{}, errors.New("missing conversation identifiers")
	}
	out := B2CResult{ResultType: string(r.ResultType), ResultCode: string(r.ResultCode), ResultDesc: r.ResultDesc,
		OriginatorConversationID: r.OriginatorConversationID, ConversationID: r.ConversationID, TransactionID: r.TransactionID}
	// ResultParameter may be a single object or an array.
	var params []kv
	if len(r.ResultParameters.ResultParameter) > 0 {
		if err := json.Unmarshal(r.ResultParameters.ResultParameter, &params); err != nil {
			var one kv
			if json.Unmarshal(r.ResultParameters.ResultParameter, &one) == nil {
				params = []kv{one}
			}
		}
	}
	for _, p := range params {
		if p.Key == "TransactionAmount" {
			out.Amount = parseMajorToMinor(string(p.Value))
		}
	}
	return out, nil
}

// NormalizeB2CResult: ResultCode 0 is success; any other code from a matched
// result callback is a definitive failure for that conversation.
func NormalizeB2CResult(r B2CResult) payments.StatusResult {
	out := payments.StatusResult{ResultCode: r.ResultCode, ResultDesc: r.ResultDesc, Receipt: r.TransactionID}
	if r.ResultCode == "0" {
		out.Outcome = payments.OutcomeSucceeded
	} else {
		out.Outcome = payments.OutcomeFailed
	}
	return out
}

// parseMajorToMinor converts "10", "10.0" or "10.00" shillings to minor
// units; returns 0 when unparseable.
func parseMajorToMinor(s string) int64 {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		frac := strings.TrimRight(s[i+1:], "0")
		s = s[:i]
		if frac != "" {
			s += "." + frac
		}
	}
	v, err := money.Parse(s, money.KES)
	if err != nil {
		return 0
	}
	return v
}
