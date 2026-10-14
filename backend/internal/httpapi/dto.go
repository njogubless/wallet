package httpapi

import (
	"time"

	"github.com/google/uuid"

	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/money"
	"github.com/sovereigntrust/wallet/internal/phone"
	"github.com/sovereigntrust/wallet/internal/transfers"
	"github.com/sovereigntrust/wallet/internal/wallet"
	"github.com/sovereigntrust/wallet/internal/withdrawals"
)

// Money is the wire representation: a decimal string in major units plus
// the ISO currency code. Clients must never use floating point for it.
type Money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func mk(minor int64, currency string) Money {
	cur, err := money.LookupCurrency(currency)
	if err != nil {
		cur = money.Currency{Code: currency, MinorUnits: 2}
	}
	return Money{Amount: money.Format(minor, cur), Currency: currency}
}

type walletDTO struct {
	ID            uuid.UUID `json:"id"`
	AccountNumber string    `json:"account_number"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

func toWallet(w wallet.Wallet) walletDTO {
	return walletDTO{ID: w.ID, AccountNumber: w.AccountNumber, Currency: w.Currency, Status: w.Status, CreatedAt: w.CreatedAt}
}

type balanceDTO struct {
	WalletID       uuid.UUID `json:"wallet_id"`
	Available      Money     `json:"available"`
	Held           Money     `json:"held"`
	Total          Money     `json:"total"`
	PendingInbound Money     `json:"pending_inbound"`
	AsOf           time.Time `json:"as_of"`
}

// Customer-facing state shared by all transaction kinds. "unknown" is shown
// when the outcome is uncertain; it is not "failed".
func displayStatus(status string, uncertain bool) string {
	switch {
	case uncertain && (status == "processing" || status == "pending"):
		return "unknown"
	case status == "created" || status == "pending_provider" || status == "pending" || status == "processing":
		return "pending"
	case status == "expired":
		return "unknown"
	}
	return status
}

type depositDTO struct {
	ID              uuid.UUID  `json:"id"`
	WalletID        uuid.UUID  `json:"wallet_id"`
	Provider        string     `json:"provider"`
	Environment     string     `json:"provider_environment"`
	Simulated       bool       `json:"simulated"`
	Phone           string     `json:"phone_masked"`
	Amount          Money      `json:"amount"`
	Status          string     `json:"status"`
	DisplayStatus   string     `json:"display_status"`
	ProviderStatus  string     `json:"provider_status"`
	ProviderReceipt *string    `json:"provider_receipt"`
	FailureReason   *string    `json:"failure_reason"`
	ExpiresAt       time.Time  `json:"expires_at"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at"`
}

func toDeposit(d *deposits.Deposit) depositDTO {
	return depositDTO{ID: d.ID, WalletID: d.WalletID, Provider: d.Provider, Environment: d.ProviderEnv, Simulated: d.ProviderEnv == "simulated",
		Phone: phone.Mask(d.MSISDN), Amount: mk(d.Amount, d.Currency), Status: d.Status, DisplayStatus: displayStatus(d.Status, false),
		ProviderStatus: d.ProviderStatus, ProviderReceipt: d.ProviderReceipt, FailureReason: d.FailureReason,
		ExpiresAt: d.ExpiresAt, CreatedAt: d.CreatedAt, CompletedAt: d.CompletedAt}
}

type withdrawalDTO struct {
	ID               uuid.UUID  `json:"id"`
	WalletID         uuid.UUID  `json:"wallet_id"`
	Provider         string     `json:"provider"`
	Environment      string     `json:"provider_environment"`
	Simulated        bool       `json:"simulated"`
	Phone            string     `json:"phone_masked"`
	Amount           Money      `json:"amount"`
	Fee              Money      `json:"fee"`
	Total            Money      `json:"total"`
	Status           string     `json:"status"`
	DisplayStatus    string     `json:"display_status"`
	OutcomeUncertain bool       `json:"outcome_uncertain"`
	ProviderReceipt  *string    `json:"provider_receipt"`
	FailureReason    *string    `json:"failure_reason"`
	CreatedAt        time.Time  `json:"created_at"`
	CompletedAt      *time.Time `json:"completed_at"`
}

func toWithdrawal(w *withdrawals.Withdrawal) withdrawalDTO {
	return withdrawalDTO{ID: w.ID, WalletID: w.WalletID, Provider: w.Provider, Environment: w.ProviderEnv, Simulated: w.ProviderEnv == "simulated",
		Phone: phone.Mask(w.MSISDN), Amount: mk(w.Amount, w.Currency), Fee: mk(w.Fee, w.Currency), Total: mk(w.Total(), w.Currency),
		Status: w.Status, DisplayStatus: displayStatus(w.Status, w.OutcomeUncertain), OutcomeUncertain: w.OutcomeUncertain,
		ProviderReceipt: w.ProviderReceipt, FailureReason: w.FailureReason, CreatedAt: w.CreatedAt, CompletedAt: w.CompletedAt}
}

type transferDTO struct {
	ID            uuid.UUID  `json:"id"`
	Direction     string     `json:"direction"`
	Counterparty  string     `json:"counterparty_name"`
	CounterAcct   string     `json:"counterparty_account"`
	Amount        Money      `json:"amount"`
	Fee           Money      `json:"fee"`
	Total         Money      `json:"total"`
	Status        string     `json:"status"`
	DisplayStatus string     `json:"display_status"`
	Description   string     `json:"description"`
	CreatedAt     time.Time  `json:"created_at"`
	CompletedAt   *time.Time `json:"completed_at"`
}

func toTransfer(t *transfers.Transfer, viewer uuid.UUID) transferDTO {
	d := transferDTO{ID: t.ID, Amount: mk(t.Amount, t.Currency), Status: t.Status, DisplayStatus: displayStatus(t.Status, false),
		Description: t.Description, CreatedAt: t.CreatedAt, CompletedAt: t.CompletedAt}
	if t.SenderUserID == viewer {
		d.Direction, d.Counterparty, d.CounterAcct = "debit", transfers.MaskName(t.RecipientName), t.RecipientAccount
		d.Fee, d.Total = mk(t.Fee, t.Currency), mk(t.Amount+t.Fee, t.Currency)
	} else {
		d.Direction, d.Counterparty, d.CounterAcct = "credit", transfers.MaskName(t.SenderName), t.SenderAccount
		d.Fee, d.Total = mk(0, t.Currency), mk(t.Amount, t.Currency)
	}
	return d
}

type activityDTO struct {
	ID            uuid.UUID  `json:"id"`
	WalletID      uuid.UUID  `json:"wallet_id"`
	Type          string     `json:"type"`
	Direction     string     `json:"direction"`
	Amount        Money      `json:"amount"`
	Fee           Money      `json:"fee"`
	Status        string     `json:"status"`
	DisplayStatus string     `json:"display_status"`
	Provider      *string    `json:"provider"`
	Counterparty  *string    `json:"counterparty"`
	Detail        *string    `json:"counterparty_detail"`
	Reference     *string    `json:"reference"`
	Description   string     `json:"description"`
	FailureReason *string    `json:"failure_reason"`
	CreatedAt     time.Time  `json:"created_at"`
	CompletedAt   *time.Time `json:"completed_at"`
}

func toActivity(a wallet.Activity) activityDTO {
	d := activityDTO{ID: a.ID, WalletID: a.WalletID, Type: a.Type, Direction: a.Direction, Amount: mk(a.Amount, a.Currency),
		Fee: mk(a.Fee, a.Currency), Status: a.Status, DisplayStatus: displayStatus(a.Status, false), Provider: a.Provider,
		Reference: a.Reference, Description: a.Description, FailureReason: a.FailureReason, CreatedAt: a.CreatedAt, CompletedAt: a.CompletedAt}
	// Mask personal data of the counterparty.
	if a.Counterparty != nil {
		n := transfers.MaskName(*a.Counterparty)
		d.Counterparty = &n
	}
	if a.CounterpartyDetail != nil {
		v := *a.CounterpartyDetail
		if len(v) > 4 && v[0] == '+' {
			v = phone.Mask(v)
		} else if len(v) == 12 {
			v = "••••" + v[8:]
		}
		d.Detail = &v
	}
	return d
}

type page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}
