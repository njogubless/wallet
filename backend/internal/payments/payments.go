// Package payments defines the provider-neutral contract that mobile-money
// adapters implement. Providers differ materially (sync vs async status,
// callback authentication, payout support); Capabilities makes those
// differences explicit so callers never assume a feature exists.
package payments

import (
	"context"
	"errors"
	"fmt"
)

type Capabilities struct {
	Collection bool `json:"collection"` // customer-initiated payment prompt (STK/USSD push)
	// CollectionStatusQuery: authenticated, synchronous status lookup.
	CollectionStatusQuery bool `json:"collection_status_query"`
	Payout                bool `json:"payout"`
	// PayoutStatusQuery: authenticated, synchronous payout status lookup.
	// Daraja B2C has none (Transaction Status API is asynchronous).
	PayoutStatusQuery bool `json:"payout_status_query"`
	// SignedCallbacks: the provider cryptographically authenticates callbacks.
	SignedCallbacks bool `json:"signed_callbacks"`
	// WholeUnitsOnly: amounts must be whole major units (e.g. whole shillings).
	WholeUnitsOnly bool `json:"whole_units_only"`
}

type CollectionRequest struct {
	Reference   string // our stable reference, also used as provider idempotency handle where supported
	MSISDN      string // E.164
	Amount      int64  // minor units
	Currency    string
	Description string
	CallbackURL string
}

type CollectionResponse struct {
	RequestRef  string // reference we sent (echo or provider-required)
	CheckoutRef string // provider-issued handle for status queries
	MerchantRef string
	ResultCode  string
	ResultDesc  string
}

type PayoutRequest struct {
	Reference  string
	MSISDN     string
	Amount     int64
	Currency   string
	Remarks    string
	ResultURL  string
	TimeoutURL string
}

type PayoutResponse struct {
	RequestRef      string
	ConversationRef string
	ResultCode      string
	ResultDesc      string
}

type Outcome string

const (
	OutcomePending   Outcome = "pending"
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	// OutcomeUnknown: the provider cannot tell us yet or the answer is ambiguous.
	OutcomeUnknown Outcome = "unknown"
)

type StatusQuery struct {
	RequestRef  string
	CheckoutRef string
	MSISDN      string
	Amount      int64
}

type StatusResult struct {
	Outcome    Outcome
	Receipt    string
	ResultCode string
	ResultDesc string
}

// Error classes. Adapters wrap provider failures in one of these so the
// application can decide whether money may have moved.
var (
	// ErrRejected: the provider definitively refused the request; nothing moved.
	ErrRejected = errors.New("provider rejected request")
	// ErrNotSent: the request never reached the provider (e.g. DNS/connect
	// failure, auth token fetch failed); safe to retry.
	ErrNotSent = errors.New("request not sent to provider")
	// ErrUncertain: the request may have been processed (timeout after send,
	// 5xx, unparseable response). Never blindly retry money-moving calls.
	ErrUncertain = errors.New("provider outcome uncertain")
	// ErrUnsupported: capability not offered by this provider.
	ErrUnsupported = errors.New("operation not supported by provider")
)

type ProviderError struct {
	Class      error
	Code       string
	Message    string
	HTTPStatus int
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("%v: %s %s (http %d)", e.Class, e.Code, e.Message, e.HTTPStatus)
}
func (e *ProviderError) Unwrap() error { return e.Class }

type Provider interface {
	Name() string        // "mpesa" | "airtel"
	Environment() string // "simulated" | "sandbox" | "production"
	Capabilities() Capabilities
	InitiateCollection(ctx context.Context, req CollectionRequest) (CollectionResponse, error)
	QueryCollection(ctx context.Context, q StatusQuery) (StatusResult, error)
	InitiatePayout(ctx context.Context, req PayoutRequest) (PayoutResponse, error)
	QueryPayout(ctx context.Context, q StatusQuery) (StatusResult, error)
}

// Registry looks up configured providers by name.
type Registry map[string]Provider

func (r Registry) Get(name string) (Provider, bool) {
	p, ok := r[name]
	return p, ok
}
