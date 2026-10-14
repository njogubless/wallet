package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/auth"
	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/money"
	"github.com/sovereigntrust/wallet/internal/notify"
	"github.com/sovereigntrust/wallet/internal/policy"
	"github.com/sovereigntrust/wallet/internal/reqctx"
	"github.com/sovereigntrust/wallet/internal/transfers"
	"github.com/sovereigntrust/wallet/internal/wallet"
	"github.com/sovereigntrust/wallet/internal/withdrawals"
)

// ---- auth ----

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone    string `json:"phone"`
		Email    string `json:"email"`
		FullName string `json:"full_name"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	u, err := s.Auth.Register(r.Context(), auth.RegisterInput{Phone: in.Phone, Email: in.Email, FullName: in.FullName, Password: in.Password})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	m := reqctx.MetaFrom(r.Context())
	sess, err := s.Auth.Login(r.Context(), u.Phone, in.Password, m.UserAgent, m.IP)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone    string `json:"phone"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	m := reqctx.MetaFrom(r.Context())
	sess, err := s.Auth.Login(r.Context(), in.Phone, in.Password, m.UserAgent, m.IP)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	m := reqctx.MetaFrom(r.Context())
	sess, err := s.Auth.Refresh(r.Context(), in.RefreshToken, m.UserAgent, m.IP)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Auth.Logout(r.Context(), in.RefreshToken); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) verifyPhone(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	u, err := s.Auth.VerifyPhone(r.Context(), principal(r).UserID, strings.TrimSpace(in.Code))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) resendPhoneCode(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.RequestPhoneVerification(r.Context(), principal(r).UserID); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone string `json:"phone"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	_ = s.Auth.ForgotPassword(r.Context(), in.Phone)
	// Same response whether or not the number is registered.
	writeJSON(w, http.StatusAccepted, map[string]string{"message": "If the number is registered, a reset code has been sent."})
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone       string `json:"phone"`
		Code        string `json:"code"`
		NewPassword string `json:"new_password"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Auth.ResetPassword(r.Context(), in.Phone, strings.TrimSpace(in.Code), in.NewPassword); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- profile ----

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, err := auth.GetUser(r.Context(), s.Pool, principal(r).UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) setPIN(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
		PIN      string `json:"pin"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Auth.SetPIN(r.Context(), principal(r).UserID, in.Password, in.PIN); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Auth.ChangePassword(r.Context(), principal(r).UserID, in.CurrentPassword, in.NewPassword); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) limits(w http.ResponseWriter, r *http.Request) {
	u, err := auth.GetUser(r.Context(), s.Pool, principal(r).UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sums, err := policy.Summaries(r.Context(), s.Pool, u.ID, u.KYCTier, money.KES.Code)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kyc_tier": u.KYCTier, "limits": sums,
		"verification": map[string]any{"phone_verified": u.PhoneVerifiedAt != nil, "email_verified": u.EmailVerifiedAt != nil}})
}

// feeQuote shows the fee and total before the user confirms.
func (s *Server) feeQuote(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	op, provider, amountStr := q.Get("operation"), q.Get("provider"), q.Get("amount")
	currency := q.Get("currency")
	if currency == "" {
		currency = money.KES.Code
	}
	var amount, fee, total int64
	switch op {
	case "transfer":
		quote, err := s.Transfers.Preview(r.Context(), amountStr, currency)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		amount, fee, total = quote.Amount, quote.Fee, quote.Total
	case "withdrawal":
		quote, err := s.Withdrawals.Preview(r.Context(), provider, amountStr, currency)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		amount, fee, total = quote.Amount, quote.Fee, quote.Total
	case "deposit":
		cur, err := money.LookupCurrency(currency)
		if err != nil {
			s.writeError(w, r, apperr.Field("currency", "unsupported currency"))
			return
		}
		amount, err = money.ParsePositive(amountStr, cur)
		if err != nil {
			s.writeError(w, r, apperr.Field("amount", err.Error()))
			return
		}
		fee, err = policy.Fee(r.Context(), s.Pool, "deposit", provider, cur.Code, amount)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		// Deposit fees (if any) are informational; the wallet is credited the full amount.
		total = amount
	default:
		s.writeError(w, r, apperr.Field("operation", "must be deposit, transfer or withdrawal"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operation": op, "amount": mk(amount, currency), "fee": mk(fee, currency), "total": mk(total, currency)})
}

// ---- wallets ----

func (s *Server) listWallets(w http.ResponseWriter, r *http.Request) {
	ws, err := wallet.ListForUser(r.Context(), s.Pool, principal(r).UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]walletDTO, 0, len(ws))
	for _, x := range ws {
		out = append(out, toWallet(x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) ownedWallet(r *http.Request) (wallet.Wallet, error) {
	id, err := pathUUID(r, "walletID")
	if err != nil {
		return wallet.Wallet{}, err
	}
	return wallet.GetOwned(r.Context(), s.Pool, id, principal(r).UserID)
}

func (s *Server) getWallet(w http.ResponseWriter, r *http.Request) {
	wl, err := s.ownedWallet(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWallet(wl))
}

func (s *Server) balanceOf(r *http.Request, wl wallet.Wallet) (balanceDTO, error) {
	b, err := wallet.GetBalance(r.Context(), s.Pool, wl)
	if err != nil {
		return balanceDTO{}, err
	}
	pending, err := wallet.PendingInbound(r.Context(), s.Pool, wl.ID)
	if err != nil {
		return balanceDTO{}, err
	}
	return balanceDTO{WalletID: wl.ID, Available: mk(b.Available, wl.Currency), Held: mk(b.Held, wl.Currency),
		Total: mk(b.Total(), wl.Currency), PendingInbound: mk(pending, wl.Currency), AsOf: time.Now().UTC()}, nil
}

func (s *Server) walletBalance(w http.ResponseWriter, r *http.Request) {
	wl, err := s.ownedWallet(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	b, err := s.balanceOf(r, wl)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func activityFilter(r *http.Request) (wallet.ActivityFilter, error) {
	q := r.URL.Query()
	f := wallet.ActivityFilter{Query: q.Get("q"), Limit: queryInt(r, "limit", 30)}
	if t := q.Get("type"); t != "" {
		for _, v := range strings.Split(t, ",") {
			switch v {
			case "deposit", "withdrawal", "transfer_in", "transfer_out", "adjustment":
				f.Types = append(f.Types, v)
			case "transfer":
				f.Types = append(f.Types, "transfer_in", "transfer_out")
			default:
				return f, apperr.Field("type", "unknown type "+v)
			}
		}
	}
	if st := q.Get("status"); st != "" {
		for _, v := range strings.Split(st, ",") {
			switch v {
			case "pending":
				f.Statuses = append(f.Statuses, "created", "pending_provider", "pending", "processing")
			case "succeeded", "failed", "reversed", "expired":
				f.Statuses = append(f.Statuses, v)
			default:
				return f, apperr.Field("status", "unknown status "+v)
			}
		}
	}
	for name, dst := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return f, apperr.Field(name, "must be RFC3339")
			}
			*dst = &t
		}
	}
	c, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		return f, err
	}
	if c != nil {
		f.BeforeTime, f.BeforeID = &c.T, &c.ID
	}
	return f, nil
}

func (s *Server) writeActivity(w http.ResponseWriter, r *http.Request, walletID uuid.UUID) {
	f, err := activityFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rows, err := wallet.ListActivity(r.Context(), s.Pool, walletID, f)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := page[activityDTO]{Data: make([]activityDTO, 0, len(rows))}
	for _, a := range rows {
		out.Data = append(out.Data, toActivity(a))
	}
	if len(rows) == f.Limit {
		last := rows[len(rows)-1]
		c := encodeCursor(last.CreatedAt, last.ID)
		out.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) walletTransactions(w http.ResponseWriter, r *http.Request) {
	wl, err := s.ownedWallet(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeActivity(w, r, wl.ID)
}

func (s *Server) primaryWallet(r *http.Request) (wallet.Wallet, error) {
	return wallet.PrimaryForUser(r.Context(), s.Pool, principal(r).UserID, money.KES.Code)
}

func (s *Server) listTransactions(w http.ResponseWriter, r *http.Request) {
	wl, err := s.primaryWallet(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeActivity(w, r, wl.ID)
}

// getTransaction returns a receipt-style detail for any activity item.
func (s *Server) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "transactionID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	wl, err := s.primaryWallet(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	a, err := wallet.GetActivity(r.Context(), s.Pool, wl.ID, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	receipt := map[string]any{
		"transaction":    toActivity(a),
		"wallet_account": wl.AccountNumber,
		"receipt_number": "ST-" + strings.ToUpper(strings.ReplaceAll(a.ID.String(), "-", "")[:12]),
	}
	switch a.Type {
	case "withdrawal":
		if wd, err := withdrawals.GetForUser(r.Context(), s.Pool, id, principal(r).UserID); err == nil {
			receipt["withdrawal"] = toWithdrawal(wd)
		}
	case "deposit":
		if d, err := deposits.GetForUser(r.Context(), s.Pool, id, principal(r).UserID); err == nil {
			receipt["deposit"] = toDeposit(d)
		}
	case "transfer_in", "transfer_out":
		if t, err := transfers.GetForUser(r.Context(), s.Pool, id, principal(r).UserID); err == nil {
			receipt["transfer"] = toTransfer(t, principal(r).UserID)
		}
	}
	writeJSON(w, http.StatusOK, receipt)
}

// ---- money movement ----

func (s *Server) createDeposit(w http.ResponseWriter, r *http.Request) {
	key, err := idempotencyKey(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in struct {
		Provider string `json:"provider"`
		Phone    string `json:"phone"`
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	d, replay, err := s.Deposits.Create(r.Context(), deposits.CreateInput{UserID: principal(r).UserID, Provider: in.Provider,
		Phone: in.Phone, Amount: in.Amount, Currency: in.Currency, IdempotencyKey: key})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	created(w, replay, toDeposit(d), http.StatusAccepted)
}

func (s *Server) getDeposit(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "depositID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	d, err := deposits.GetForUser(r.Context(), s.Pool, id, principal(r).UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDeposit(d))
}

func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	key, err := idempotencyKey(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in struct {
		RecipientPhone   string `json:"recipient_phone"`
		RecipientAccount string `json:"recipient_account"`
		Amount           string `json:"amount"`
		Currency         string `json:"currency"`
		Description      string `json:"description"`
		PIN              string `json:"pin"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	t, replay, err := s.Transfers.Create(r.Context(), transfers.CreateInput{UserID: principal(r).UserID, RecipientPhone: in.RecipientPhone,
		RecipientAccount: in.RecipientAccount, Amount: in.Amount, Currency: in.Currency, Description: in.Description, PIN: in.PIN, IdempotencyKey: key})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	created(w, replay, toTransfer(t, principal(r).UserID), http.StatusCreated)
}

func (s *Server) getTransfer(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "transferID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	t, err := transfers.GetForUser(r.Context(), s.Pool, id, principal(r).UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransfer(t, principal(r).UserID))
}

func (s *Server) lookupRecipient(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rec, err := transfers.LookupRecipient(r.Context(), s.Pool, q.Get("phone"), q.Get("account_number"), money.KES.Code)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if rec.UserID == principal(r).UserID {
		s.writeError(w, r, apperr.Validation("you cannot transfer to your own wallet", nil))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) createWithdrawal(w http.ResponseWriter, r *http.Request) {
	key, err := idempotencyKey(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var in struct {
		Provider string `json:"provider"`
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
		PIN      string `json:"pin"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	wd, replay, err := s.Withdrawals.Create(r.Context(), withdrawals.CreateInput{UserID: principal(r).UserID, Provider: in.Provider,
		Amount: in.Amount, Currency: in.Currency, PIN: in.PIN, IdempotencyKey: key})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	created(w, replay, toWithdrawal(wd), http.StatusAccepted)
}

func (s *Server) getWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "withdrawalID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	wd, err := withdrawals.GetForUser(r.Context(), s.Pool, id, principal(r).UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWithdrawal(wd))
}

// ---- statements & notifications ----

func (s *Server) listStatements(w http.ResponseWriter, r *http.Request) {
	list, err := s.Statements.List(r.Context(), principal(r).UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

func (s *Server) createStatement(w http.ResponseWriter, r *http.Request) {
	var in struct {
		From   string `json:"from"`
		To     string `json:"to"`
		Format string `json:"format"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	from, err1 := time.Parse("2006-01-02", in.From)
	to, err2 := time.Parse("2006-01-02", in.To)
	if err1 != nil || err2 != nil {
		s.writeError(w, r, apperr.Validation("from and to must be YYYY-MM-DD dates", nil))
		return
	}
	st, err := s.Statements.Generate(r.Context(), principal(r).UserID, from, to, in.Format)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, st)
}

func (s *Server) downloadStatement(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "statementID")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	format, content, err := s.Statements.Download(r.Context(), principal(r).UserID, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ct := map[string]string{"csv": "text/csv; charset=utf-8", "pdf": "application/pdf"}[format]
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", `attachment; filename="statement-`+id.String()[:8]+`.`+format+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	_, _ = w.Write(content)
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	var before *time.Time
	if v := r.URL.Query().Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			s.writeError(w, r, apperr.Field("before", "must be RFC3339"))
			return
		}
		before = &t
	}
	list, unread, err := notify.List(r.Context(), s.Pool, principal(r).UserID, before, queryInt(r, "limit", 30))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list, "unread_count": unread})
}

func (s *Server) readNotifications(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID *uuid.UUID `json:"id"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := notify.MarkRead(r.Context(), s.Pool, principal(r).UserID, in.ID); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
