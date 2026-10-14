// Package simulator is a deterministic, stateless stand-in for a mobile-money
// provider, for local development and automated tests. It never contacts a
// real provider and never moves real money. Records it produces are marked
// provider_env = 'simulated' and receipts are prefixed "SIM".
//
// Outcomes are selected by the last four digits of the customer number:
//
//	...0000  provider rejects the request outright
//	...1111  customer cancels / payout fails (definitive failure)
//	...2222  request times out after sending (uncertain outcome)
//	...3333  stays pending forever (never resolves)
//	other    succeeds once SettleAfter has elapsed since initiation
package simulator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sovereigntrust/wallet/internal/payments"
)

type Provider struct {
	ProviderName string
	SettleAfter  time.Duration
	Now          func() time.Time
}

func New(name string) *Provider {
	return &Provider{ProviderName: name, SettleAfter: 3 * time.Second, Now: time.Now}
}

func (p *Provider) Name() string        { return p.ProviderName }
func (p *Provider) Environment() string { return "simulated" }

func (p *Provider) Capabilities() payments.Capabilities {
	return payments.Capabilities{Collection: true, CollectionStatusQuery: true, Payout: true, PayoutStatusQuery: true, WholeUnitsOnly: true}
}

func suffix(msisdn string) string {
	if len(msisdn) < 4 {
		return ""
	}
	return msisdn[len(msisdn)-4:]
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

// The handle encodes the initiation time so status queries are stateless.
func (p *Provider) handle(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, p.Now().UnixMilli(), randHex(4))
}

func (p *Provider) elapsed(handle string) (time.Duration, bool) {
	parts := strings.Split(handle, "-")
	if len(parts) != 3 {
		return 0, false
	}
	ms, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return p.Now().Sub(time.UnixMilli(ms)), true
}

func (p *Provider) initiate(msisdn, prefix string) (string, error) {
	switch suffix(msisdn) {
	case "0000":
		return "", &payments.ProviderError{Class: payments.ErrRejected, Code: "SIM400", Message: "simulated rejection", HTTPStatus: 400}
	case "2222":
		return "", &payments.ProviderError{Class: payments.ErrUncertain, Code: "SIMTIMEOUT", Message: "simulated timeout after send"}
	}
	return p.handle(prefix), nil
}

func (p *Provider) query(handle, msisdn string) (payments.StatusResult, error) {
	el, ok := p.elapsed(handle)
	if !ok {
		return payments.StatusResult{Outcome: payments.OutcomeUnknown, ResultDesc: "unknown handle"}, nil
	}
	if el < p.SettleAfter || suffix(msisdn) == "3333" {
		return payments.StatusResult{Outcome: payments.OutcomePending, ResultCode: "SIMPENDING", ResultDesc: "awaiting customer"}, nil
	}
	if suffix(msisdn) == "1111" {
		return payments.StatusResult{Outcome: payments.OutcomeFailed, ResultCode: "1032", ResultDesc: "Request cancelled by user (simulated)"}, nil
	}
	return payments.StatusResult{Outcome: payments.OutcomeSucceeded, Receipt: "SIM" + randHex(4), ResultCode: "0", ResultDesc: "Success (simulated)"}, nil
}

func (p *Provider) InitiateCollection(_ context.Context, req payments.CollectionRequest) (payments.CollectionResponse, error) {
	h, err := p.initiate(req.MSISDN, "SIMCO")
	if err != nil {
		return payments.CollectionResponse{}, err
	}
	return payments.CollectionResponse{RequestRef: req.Reference, CheckoutRef: h, MerchantRef: "SIMM-" + randHex(4), ResultCode: "0", ResultDesc: "Accepted (simulated)"}, nil
}

func (p *Provider) QueryCollection(_ context.Context, q payments.StatusQuery) (payments.StatusResult, error) {
	return p.query(q.CheckoutRef, q.MSISDN)
}

func (p *Provider) InitiatePayout(_ context.Context, req payments.PayoutRequest) (payments.PayoutResponse, error) {
	h, err := p.initiate(req.MSISDN, "SIMPO")
	if err != nil {
		return payments.PayoutResponse{}, err
	}
	return payments.PayoutResponse{RequestRef: req.Reference, ConversationRef: h, ResultCode: "0", ResultDesc: "Accepted (simulated)"}, nil
}

func (p *Provider) QueryPayout(_ context.Context, q payments.StatusQuery) (payments.StatusResult, error) {
	return p.query(q.CheckoutRef, q.MSISDN)
}
