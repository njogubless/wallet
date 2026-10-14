package httpapi

import (
	"crypto/subtle"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sovereigntrust/wallet/internal/admin"
	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/audit"
	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/money"
	"github.com/sovereigntrust/wallet/internal/policy"
	"github.com/sovereigntrust/wallet/internal/reconciliation"
	"github.com/sovereigntrust/wallet/internal/reqctx"
	"github.com/sovereigntrust/wallet/internal/transfers"
	"github.com/sovereigntrust/wallet/internal/wallet"
	"github.com/sovereigntrust/wallet/internal/withdrawals"
)

// ---- provider callbacks ----

func ipAllowed(ip string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	parsed := net.ParseIP(ip)
	for _, a := range allow {
		if strings.Contains(a, "/") {
			if _, n, err := net.ParseCIDR(a); err == nil && parsed != nil && n.Contains(parsed) {
				return true
			}
		} else if a == ip {
			return true
		}
	}
	return false
}

func (s *Server) callback(provider, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		if s.Cfg.CallbackToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.Cfg.CallbackToken)) != 1 {
			// Indistinguishable from an unknown route.
			s.writeError(w, r, apperr.NotFound("route"))
			return
		}
		allow := s.Cfg.Mpesa.CallbackAllowlist
		if provider == "airtel" {
			allow = s.Cfg.Airtel.CallbackAllowlist
		}
		ip := reqctx.MetaFrom(r.Context()).IP
		if !ipAllowed(ip, allow) {
			s.Log.Warn("callback from non-allowlisted IP rejected", "provider", provider, "ip", ip)
			s.writeError(w, r, apperr.ErrForbidden)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			s.writeError(w, r, apperr.Validation("body too large", nil))
			return
		}
		if _, err := s.Callbacks.Save(r.Context(), provider, kind, raw, ip); err != nil {
			s.writeError(w, r, err)
			return
		}
		if provider == "mpesa" {
			writeJSON(w, http.StatusOK, map[string]any{"ResultCode": 0, "ResultDesc": "Accepted"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "received"})
	}
}

// ---- admin ----

func (s *Server) adminDashboard(w http.ResponseWriter, r *http.Request) {
	d, err := s.Admin.Dashboard(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) adminSearchUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Admin.SearchUsers(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("status"), queryInt(r, "limit", 25))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	_ = audit.Record(r.Context(), s.Pool, audit.Event{Action: "admin.user_search", Details: map[string]any{"q": r.URL.Query().Get("q")}})
	writeJSON(w, http.StatusOK, map[string]any{"data": users})
}

func (s *Server) adminGetUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "userID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	d, err := s.Admin.GetUser(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	_ = audit.Record(r.Context(), s.Pool, audit.Event{Action: "admin.user_viewed", TargetType: "user", TargetID: id.String()})
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) adminSetUserStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "userID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Admin.SetUserStatus(r.Context(), principal(r).UserID, id, in.Status, in.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	d, err := s.Admin.GetUser(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) adminWalletTransactions(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "walletID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, err := wallet.Get(r.Context(), s.Pool, id); err != nil {
		s.writeError(w, r, err)
		return
	}
	_ = audit.Record(r.Context(), s.Pool, audit.Event{Action: "admin.wallet_transactions_viewed", TargetType: "wallet", TargetID: id.String()})
	s.writeActivity(w, r, id)
}

func (s *Server) adminPayments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := s.Admin.ListPayments(r.Context(), admin.PaymentFilter{Kind: q.Get("kind"), Status: q.Get("status"),
		Provider: q.Get("provider"), NeedsReview: q.Get("needs_review") == "true", Limit: queryInt(r, "limit", 50)})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if rows == nil {
		rows = []admin.PaymentRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows})
}

func (s *Server) adminGetTransfer(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "transferID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	t, err := transfers.Get(r.Context(), s.Pool, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": t.ID, "sender_wallet_id": t.SenderWalletID, "recipient_wallet_id": t.RecipientWalletID,
		"sender_name": t.SenderName, "recipient_name": t.RecipientName, "amount": mk(t.Amount, t.Currency), "fee": mk(t.Fee, t.Currency),
		"status": t.Status, "description": t.Description, "journal_id": t.JournalID, "reversal_journal_id": t.ReversalJournalID, "created_at": t.CreatedAt})
}

func (s *Server) adminRecheckDeposit(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "depositID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, err := deposits.Get(r.Context(), s.Pool, id); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Deposits.RequestRecheck(r.Context(), s.Pool, id); err != nil {
		s.writeError(w, r, err)
		return
	}
	_ = audit.Record(r.Context(), s.Pool, audit.Event{Action: "admin.deposit_recheck", TargetType: "deposit", TargetID: id.String()})
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) adminRecheckWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "withdrawalID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, err := withdrawals.Get(r.Context(), s.Pool, id); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := withdrawals.RequestRecheck(r.Context(), s.Pool, id); err != nil {
		s.writeError(w, r, err)
		return
	}
	_ = audit.Record(r.Context(), s.Pool, audit.Event{Action: "admin.withdrawal_recheck", TargetType: "withdrawal", TargetID: id.String()})
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) adminResolveWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "withdrawalID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in struct {
		Outcome         string `json:"outcome"`
		ProviderReceipt string `json:"provider_receipt"`
		Evidence        string `json:"evidence"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	wd, err := s.Withdrawals.Resolve(r.Context(), id, in.Outcome, in.ProviderReceipt, in.Evidence)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWithdrawal(wd))
}

func (s *Server) adminListFinancialRequests(w http.ResponseWriter, r *http.Request) {
	list, err := s.Admin.ListFinancialRequests(r.Context(), r.URL.Query().Get("status"), queryInt(r, "limit", 50))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

func (s *Server) adminCreateFinancialRequest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind       string     `json:"kind"`
		WalletID   *uuid.UUID `json:"wallet_id"`
		TargetID   *uuid.UUID `json:"target_id"`
		Amount     string     `json:"amount"`
		Currency   string     `json:"currency"`
		ReasonCode string     `json:"reason_code"`
		Memo       string     `json:"memo"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.Admin.CreateFinancialRequest(r.Context(), principal(r).UserID, admin.CreateRequestInput{Kind: in.Kind, WalletID: in.WalletID,
		TargetID: in.TargetID, Amount: in.Amount, Currency: in.Currency, ReasonCode: in.ReasonCode, Memo: in.Memo})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) adminDecide(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathUUID(r, "requestID")
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		var in struct {
			Note string `json:"note"`
		}
		if err := decode(w, r, &in); err != nil {
			s.writeError(w, r, err)
			return
		}
		f, err := s.Admin.Decide(r.Context(), principal(r).UserID, id, approve, in.Note)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, f)
	}
}

func (s *Server) adminListFees(w http.ResponseWriter, r *http.Request) {
	rules, err := policy.FeeHistory(r.Context(), s.Pool)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rules})
}

func (s *Server) adminAddFee(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Operation     string     `json:"operation"`
		Provider      string     `json:"provider"`
		Currency      string     `json:"currency"`
		FlatFee       string     `json:"flat_fee"`
		RateBps       int        `json:"rate_bps"`
		MinFee        string     `json:"min_fee"`
		MaxFee        *string    `json:"max_fee"`
		EffectiveFrom *time.Time `json:"effective_from"`
		Reason        string     `json:"reason"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if in.Currency == "" {
		in.Currency = money.KES.Code
	}
	cur, err := money.LookupCurrency(in.Currency)
	if err != nil {
		s.writeError(w, r, apperr.Field("currency", "unsupported currency"))
		return
	}
	parse := func(field, v string) (int64, bool) {
		if v == "" {
			return 0, true
		}
		n, err := money.Parse(v, cur)
		if err != nil {
			s.writeError(w, r, apperr.Field(field, err.Error()))
			return 0, false
		}
		return n, true
	}
	rule := policy.FeeRule{Operation: in.Operation, Provider: in.Provider, Currency: cur.Code, RateBps: in.RateBps, Reason: in.Reason}
	var ok bool
	if rule.FlatFee, ok = parse("flat_fee", in.FlatFee); !ok {
		return
	}
	if rule.MinFee, ok = parse("min_fee", in.MinFee); !ok {
		return
	}
	if in.MaxFee != nil {
		v, ok := parse("max_fee", *in.MaxFee)
		if !ok {
			return
		}
		rule.MaxFee = &v
	}
	if in.EffectiveFrom != nil {
		rule.EffectiveFrom = *in.EffectiveFrom
	}
	out, err := s.Admin.AddFeeRule(r.Context(), principal(r).UserID, rule)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) adminListLimits(w http.ResponseWriter, r *http.Request) {
	rules, err := policy.LimitHistory(r.Context(), s.Pool)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rules})
}

func (s *Server) adminAddLimit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		KYCTier       int        `json:"kyc_tier"`
		Operation     string     `json:"operation"`
		Currency      string     `json:"currency"`
		PerTxnMin     string     `json:"per_transaction_min"`
		PerTxnMax     string     `json:"per_transaction_max"`
		DailyMax      string     `json:"daily_max"`
		EffectiveFrom *time.Time `json:"effective_from"`
		Reason        string     `json:"reason"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if in.Currency == "" {
		in.Currency = money.KES.Code
	}
	cur, err := money.LookupCurrency(in.Currency)
	if err != nil {
		s.writeError(w, r, apperr.Field("currency", "unsupported currency"))
		return
	}
	rule := policy.LimitRule{KYCTier: in.KYCTier, Operation: in.Operation, Currency: cur.Code, Reason: in.Reason}
	for field, dst := range map[string]*int64{"per_transaction_min": &rule.PerTxnMin, "per_transaction_max": &rule.PerTxnMax, "daily_max": &rule.DailyMax} {
		v := map[string]string{"per_transaction_min": in.PerTxnMin, "per_transaction_max": in.PerTxnMax, "daily_max": in.DailyMax}[field]
		n, err := money.ParsePositive(v, cur)
		if err != nil {
			s.writeError(w, r, apperr.Field(field, err.Error()))
			return
		}
		*dst = n
	}
	if in.EffectiveFrom != nil {
		rule.EffectiveFrom = *in.EffectiveFrom
	}
	out, err := s.Admin.AddLimitRule(r.Context(), principal(r).UserID, rule)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) adminListReconRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Reconciliation.ListRuns(r.Context(), queryInt(r, "limit", 20))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": runs})
}

func (s *Server) adminRunRecon(w http.ResponseWriter, r *http.Request) {
	actor := principal(r).UserID
	run, err := s.Reconciliation.RunInternal(r.Context(), &actor)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// adminImportStatement accepts the normalized statement CSV as the request body.
func (s *Server) adminImportStatement(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	if provider != "mpesa" && provider != "airtel" {
		s.writeError(w, r, apperr.Field("provider", "must be mpesa or airtel"))
		return
	}
	rows, err := reconciliation.ParseStatementCSV(http.MaxBytesReader(w, r.Body, 10<<20), 50_000)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	actor := principal(r).UserID
	run, err := s.Reconciliation.ImportStatement(r.Context(), provider, rows, &actor)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) adminListReconItems(w http.ResponseWriter, r *http.Request) {
	items, err := s.Reconciliation.ListItems(r.Context(), r.URL.Query().Get("status"), r.URL.Query().Get("discrepancy"), queryInt(r, "limit", 50))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items})
}

func (s *Server) adminUpdateReconItem(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "itemID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.Reconciliation.UpdateItem(r.Context(), id, in.Status, in.Note, principal(r).UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) adminAuditEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := audit.Filter{Action: q.Get("action"), TargetType: q.Get("target_type"), TargetID: q.Get("target_id"), Limit: queryInt(r, "limit", 50)}
	if v := q.Get("actor_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			s.writeError(w, r, apperr.Field("actor_id", "invalid UUID"))
			return
		}
		f.ActorID = &id
	}
	f.BeforeID = int64(queryInt(r, "before_id", 0))
	rows, err := audit.List(r.Context(), s.Pool, f)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if rows == nil {
		rows = []audit.Row{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows})
}

func (s *Server) adminProviderHealth(w http.ResponseWriter, r *http.Request) {
	out := []admin.ProviderHealth{}
	for _, name := range []string{"mpesa", "airtel"} {
		p, ok := s.Providers.Get(name)
		if !ok {
			continue
		}
		h, err := s.Admin.ProviderStats(r.Context(), name)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		h.Environment, h.Capabilities = p.Environment(), p.Capabilities()
		out = append(out, h)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}
