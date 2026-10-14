// Package fake provides a programmable payments.Provider test double.
package fake

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/sovereigntrust/wallet/internal/payments"
)

type Provider struct {
	ProviderName string
	Caps         payments.Capabilities

	mu          sync.Mutex
	InitErr     error
	QueryResult payments.StatusResult
	QueryErr    error

	Initiations atomic.Int64
	Queries     atomic.Int64
	LastRef     string
}

func New(name string) *Provider {
	return &Provider{ProviderName: name, Caps: payments.Capabilities{
		Collection: true, CollectionStatusQuery: true, Payout: true, PayoutStatusQuery: true, WholeUnitsOnly: true,
	}, QueryResult: payments.StatusResult{Outcome: payments.OutcomePending}}
}

func (p *Provider) Name() string                        { return p.ProviderName }
func (p *Provider) Environment() string                 { return "simulated" }
func (p *Provider) Capabilities() payments.Capabilities { return p.Caps }

func (p *Provider) SetInitErr(err error) { p.mu.Lock(); p.InitErr = err; p.mu.Unlock() }
func (p *Provider) SetQuery(r payments.StatusResult, err error) {
	p.mu.Lock()
	p.QueryResult, p.QueryErr = r, err
	p.mu.Unlock()
}

func (p *Provider) InitiateCollection(_ context.Context, req payments.CollectionRequest) (payments.CollectionResponse, error) {
	p.Initiations.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.LastRef = req.Reference
	if p.InitErr != nil {
		return payments.CollectionResponse{}, p.InitErr
	}
	return payments.CollectionResponse{RequestRef: req.Reference, CheckoutRef: "CO-" + uuid.NewString(), MerchantRef: "M-1", ResultCode: "0"}, nil
}

func (p *Provider) QueryCollection(context.Context, payments.StatusQuery) (payments.StatusResult, error) {
	p.Queries.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.QueryResult, p.QueryErr
}

func (p *Provider) InitiatePayout(_ context.Context, req payments.PayoutRequest) (payments.PayoutResponse, error) {
	p.Initiations.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.LastRef = req.Reference
	if p.InitErr != nil {
		return payments.PayoutResponse{}, p.InitErr
	}
	return payments.PayoutResponse{RequestRef: req.Reference, ConversationRef: "AG-" + uuid.NewString(), ResultCode: "0"}, nil
}

func (p *Provider) QueryPayout(ctx context.Context, q payments.StatusQuery) (payments.StatusResult, error) {
	if !p.Caps.PayoutStatusQuery {
		return payments.StatusResult{}, payments.ErrUnsupported
	}
	return p.QueryCollection(ctx, q)
}
