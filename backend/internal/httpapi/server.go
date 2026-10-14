// Package httpapi exposes the versioned REST API (/api/v1).
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/sovereigntrust/wallet/internal/admin"
	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/auth"
	"github.com/sovereigntrust/wallet/internal/callbacks"
	"github.com/sovereigntrust/wallet/internal/config"
	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/payments"
	"github.com/sovereigntrust/wallet/internal/ratelimit"
	"github.com/sovereigntrust/wallet/internal/reconciliation"
	"github.com/sovereigntrust/wallet/internal/reqctx"
	"github.com/sovereigntrust/wallet/internal/statements"
	"github.com/sovereigntrust/wallet/internal/transfers"
	"github.com/sovereigntrust/wallet/internal/withdrawals"
)

type Server struct {
	Cfg            *config.Config
	Pool           *pgxpool.Pool
	Redis          *redis.Client // optional
	Log            *slog.Logger
	Auth           *auth.Service
	Tokens         *auth.TokenIssuer
	Transfers      *transfers.Service
	Deposits       *deposits.Service
	Withdrawals    *withdrawals.Service
	Callbacks      *callbacks.Store
	Statements     *statements.Service
	Admin          *admin.Service
	Reconciliation *reconciliation.Service
	Providers      payments.Registry
	Limiter        ratelimit.Limiter
}

var (
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wallet_http_requests_total", Help: "HTTP requests by route pattern, method and status.",
	}, []string{"route", "method", "status"})
	httpLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "wallet_http_request_duration_seconds", Help: "HTTP request latency by route pattern.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"route", "method"})
)

func init() { prometheus.MustRegister(httpRequests, httpLatency) }

// Handler builds the full HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	user := func(h http.HandlerFunc) http.Handler { return s.authenticate(h) }
	staff := func(h http.HandlerFunc) http.Handler { return s.authenticate(s.requireRole(h, "support", "admin")) }
	adminOnly := func(h http.HandlerFunc) http.Handler { return s.authenticate(s.requireRole(h, "admin")) }
	limited := func(name string, n int, per time.Duration, h http.Handler) http.Handler {
		return s.rateLimit(name, n, per, h)
	}
	// userLimited authenticates first so limits apply per user, not per IP.
	userLimited := func(name string, n int, per time.Duration, h http.HandlerFunc) http.Handler {
		return s.authenticate(s.rateLimit(name, n, per, h))
	}

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /api/v1/meta", s.meta)

	// Authentication
	mux.Handle("POST /api/v1/auth/register", limited("register", 5, time.Hour, http.HandlerFunc(s.register)))
	mux.Handle("POST /api/v1/auth/login", limited("login", 10, 15*time.Minute, http.HandlerFunc(s.login)))
	mux.Handle("POST /api/v1/auth/refresh", limited("refresh", 30, time.Minute, http.HandlerFunc(s.refresh)))
	mux.HandleFunc("POST /api/v1/auth/logout", s.logout)
	mux.Handle("POST /api/v1/auth/verify-phone", userLimited("verify", 10, 15*time.Minute, s.verifyPhone))
	mux.Handle("POST /api/v1/auth/verify-phone/resend", userLimited("otp", 3, 15*time.Minute, s.resendPhoneCode))
	mux.Handle("POST /api/v1/auth/forgot-password", limited("forgot", 3, 15*time.Minute, http.HandlerFunc(s.forgotPassword)))
	mux.Handle("POST /api/v1/auth/reset-password", limited("reset", 10, 15*time.Minute, http.HandlerFunc(s.resetPassword)))

	// Profile & security
	mux.Handle("GET /api/v1/me", user(s.me))
	mux.Handle("POST /api/v1/me/pin", userLimited("pin", 5, 15*time.Minute, s.setPIN))
	mux.Handle("POST /api/v1/me/password", userLimited("password", 5, 15*time.Minute, s.changePassword))
	mux.Handle("GET /api/v1/limits", user(s.limits))
	mux.Handle("GET /api/v1/fees/quote", user(s.feeQuote))

	// Wallets & transactions
	mux.Handle("GET /api/v1/wallets", user(s.listWallets))
	mux.Handle("GET /api/v1/wallets/{walletID}", user(s.getWallet))
	mux.Handle("GET /api/v1/wallets/{walletID}/balance", user(s.walletBalance))
	mux.Handle("GET /api/v1/wallets/{walletID}/transactions", user(s.walletTransactions))
	mux.Handle("GET /api/v1/transactions", user(s.listTransactions))
	mux.Handle("GET /api/v1/transactions/{transactionID}", user(s.getTransaction))

	// Money movement
	mux.Handle("POST /api/v1/deposits", userLimited("money", 20, time.Minute, s.createDeposit))
	mux.Handle("GET /api/v1/deposits/{depositID}", user(s.getDeposit))
	mux.Handle("POST /api/v1/transfers", userLimited("money", 20, time.Minute, s.createTransfer))
	mux.Handle("GET /api/v1/transfers/{transferID}", user(s.getTransfer))
	mux.Handle("GET /api/v1/recipients/lookup", userLimited("lookup", 30, 10*time.Minute, s.lookupRecipient))
	mux.Handle("POST /api/v1/withdrawals", userLimited("money", 20, time.Minute, s.createWithdrawal))
	mux.Handle("GET /api/v1/withdrawals/{withdrawalID}", user(s.getWithdrawal))

	// Statements & notifications
	mux.Handle("GET /api/v1/statements", user(s.listStatements))
	mux.Handle("POST /api/v1/statements", userLimited("statements", 10, time.Hour, s.createStatement))
	mux.Handle("GET /api/v1/statements/{statementID}/download", user(s.downloadStatement))
	mux.Handle("GET /api/v1/notifications", user(s.listNotifications))
	mux.Handle("POST /api/v1/notifications/read", user(s.readNotifications))

	// Provider callbacks (secret path token; see docs/providers)
	mux.HandleFunc("POST /api/v1/callbacks/mpesa/{token}/stk", s.callback("mpesa", callbacks.KindSTK))
	mux.HandleFunc("POST /api/v1/callbacks/mpesa/{token}/b2c/result", s.callback("mpesa", callbacks.KindB2CResult))
	mux.HandleFunc("POST /api/v1/callbacks/mpesa/{token}/b2c/timeout", s.callback("mpesa", callbacks.KindB2CTimeout))
	mux.HandleFunc("POST /api/v1/callbacks/airtel/{token}", s.callback("airtel", callbacks.KindAirtel))

	// Administration
	mux.Handle("GET /api/v1/admin/dashboard", staff(s.adminDashboard))
	mux.Handle("GET /api/v1/admin/users", staff(s.adminSearchUsers))
	mux.Handle("GET /api/v1/admin/users/{userID}", staff(s.adminGetUser))
	mux.Handle("POST /api/v1/admin/users/{userID}/status", adminOnly(s.adminSetUserStatus))
	mux.Handle("GET /api/v1/admin/wallets/{walletID}/transactions", staff(s.adminWalletTransactions))
	mux.Handle("GET /api/v1/admin/payments", staff(s.adminPayments))
	mux.Handle("GET /api/v1/admin/transfers/{transferID}", staff(s.adminGetTransfer))
	mux.Handle("POST /api/v1/admin/deposits/{depositID}/recheck", adminOnly(s.adminRecheckDeposit))
	mux.Handle("POST /api/v1/admin/withdrawals/{withdrawalID}/recheck", adminOnly(s.adminRecheckWithdrawal))
	mux.Handle("POST /api/v1/admin/withdrawals/{withdrawalID}/resolve", adminOnly(s.adminResolveWithdrawal))
	mux.Handle("GET /api/v1/admin/financial-requests", staff(s.adminListFinancialRequests))
	mux.Handle("POST /api/v1/admin/financial-requests", adminOnly(s.adminCreateFinancialRequest))
	mux.Handle("POST /api/v1/admin/financial-requests/{requestID}/approve", adminOnly(s.adminDecide(true)))
	mux.Handle("POST /api/v1/admin/financial-requests/{requestID}/reject", adminOnly(s.adminDecide(false)))
	mux.Handle("GET /api/v1/admin/settings/fees", staff(s.adminListFees))
	mux.Handle("POST /api/v1/admin/settings/fees", adminOnly(s.adminAddFee))
	mux.Handle("GET /api/v1/admin/settings/limits", staff(s.adminListLimits))
	mux.Handle("POST /api/v1/admin/settings/limits", adminOnly(s.adminAddLimit))
	mux.Handle("GET /api/v1/admin/reconciliation/runs", staff(s.adminListReconRuns))
	mux.Handle("POST /api/v1/admin/reconciliation/runs", adminOnly(s.adminRunRecon))
	mux.Handle("POST /api/v1/admin/reconciliation/statements", adminOnly(s.adminImportStatement))
	mux.Handle("GET /api/v1/admin/reconciliation/items", staff(s.adminListReconItems))
	mux.Handle("POST /api/v1/admin/reconciliation/items/{itemID}", adminOnly(s.adminUpdateReconItem))
	mux.Handle("GET /api/v1/admin/audit-events", staff(s.adminAuditEvents))
	mux.Handle("GET /api/v1/admin/providers/health", staff(s.adminProviderHealth))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, apperr.NotFound("route"))
	})

	var h http.Handler = mux
	h = s.instrument(mux, h)
	h = securityHeaders(h)
	h = s.cors(h)
	h = s.recoverer(h)
	h = s.requestMeta(h)
	return h
}

// ---- middleware ----

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) clientIP(r *http.Request) string {
	if s.Cfg.TrustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) requestMeta(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if len(rid) < 8 || len(rid) > 64 || strings.ContainsAny(rid, " \t\r\n") {
			rid = newRequestID()
		}
		w.Header().Set("X-Request-ID", rid)
		ua := r.UserAgent()
		if len(ua) > 256 {
			ua = ua[:256]
		}
		ctx := reqctx.WithMeta(r.Context(), reqctx.Meta{RequestID: rid, IP: s.clientIP(r), UserAgent: ua})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.Log.Error("panic", "panic", fmt.Sprint(p), "stack", string(debug.Stack()), "request_id", reqctx.MetaFrom(r.Context()).RequestID)
				s.writeError(w, r, fmt.Errorf("panic: %v", p))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }

// instrument logs and records metrics per route pattern (never raw paths, so
// IDs and callback tokens do not leak into metrics or logs).
func (s *Server) instrument(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		_, pattern := mux.Handler(r)
		if pattern == "" {
			pattern = "unmatched"
		}
		route := strings.Replace(pattern, "{token}", "*", 1)
		d := time.Since(start)
		httpRequests.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
		httpLatency.WithLabelValues(route, r.Method).Observe(d.Seconds())
		m := reqctx.MetaFrom(r.Context())
		attrs := []any{"method", r.Method, "route", route, "status", rec.status, "duration_ms", d.Milliseconds(), "request_id", m.RequestID, "ip", m.IP}
		if p, ok := reqctx.PrincipalFrom(r.Context()); ok {
			attrs = append(attrs, "user_id", p.UserID)
		}
		if rec.status >= 500 {
			s.Log.Error("request", attrs...)
		} else {
			s.Log.Info("request", attrs...)
		}
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || tok == "" {
			s.writeError(w, r, apperr.ErrUnauthenticated)
			return
		}
		uid, role, err := s.Tokens.Verify(tok)
		if err != nil {
			s.writeError(w, r, apperr.New(apperr.CodeUnauthenticated, "invalid or expired access token"))
			return
		}
		// Re-check account status on every request so suspensions and role
		// changes take effect before the access token expires.
		var status, currentRole string
		if err := s.Pool.QueryRow(r.Context(), `SELECT status, role FROM users WHERE id = $1`, uid).Scan(&status, &currentRole); err != nil {
			s.writeError(w, r, apperr.New(apperr.CodeUnauthenticated, "invalid or expired access token"))
			return
		}
		if status == "suspended" || status == "closed" {
			s.writeError(w, r, apperr.New(apperr.CodeAccountRestricted, "this account is "+status))
			return
		}
		if currentRole != role {
			role = currentRole
		}
		ctx := reqctx.WithPrincipal(r.Context(), reqctx.Principal{UserID: uid, Role: role})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) requireRole(next http.Handler, roles ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		for _, role := range roles {
			if p.Role == role {
				next.ServeHTTP(w, r)
				return
			}
		}
		s.writeError(w, r, apperr.ErrForbidden)
	})
}

// rateLimit keys by user when authenticated, otherwise by client IP.
func (s *Server) rateLimit(name string, limit int, window time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := name + ":ip:" + reqctx.MetaFrom(r.Context()).IP
		if p, ok := reqctx.PrincipalFrom(r.Context()); ok {
			key = name + ":user:" + p.UserID.String()
		}
		if !s.Limiter.Allow(r.Context(), key, limit, window) {
			s.writeError(w, r, apperr.New(apperr.CodeRateLimited, "too many requests; slow down"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cors allows only explicitly configured origins. Wildcards are never used
// because requests carry bearer tokens.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.Cfg.CORSAllowedOrigins {
		allowed[strings.TrimRight(o, "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "X-Request-ID, Idempotent-Replayed, Retry-After, Content-Disposition")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- health ----

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{"database": "ok"}
	status := http.StatusOK
	if err := s.Pool.Ping(ctx); err != nil {
		checks["database"], status = "unavailable", http.StatusServiceUnavailable
	}
	if s.Redis != nil {
		checks["redis"] = "ok"
		if err := s.Redis.Ping(ctx).Err(); err != nil {
			checks["redis"] = "degraded" // rate limiting falls back to memory
		}
	}
	writeJSON(w, status, map[string]any{"status": map[bool]string{true: "ready", false: "not_ready"}[status == 200], "checks": checks})
}

func (s *Server) meta(w http.ResponseWriter, _ *http.Request) {
	providers := []map[string]any{}
	for _, name := range []string{"mpesa", "airtel"} {
		if p, ok := s.Providers.Get(name); ok {
			providers = append(providers, map[string]any{"name": name, "environment": p.Environment(), "capabilities": p.Capabilities()})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_version": "v1", "payments_mode": s.Cfg.PaymentsMode, "currencies": []map[string]any{{"code": "KES", "minor_units": 2}},
		"providers": providers, "phone_verification_required": s.Cfg.RequirePhoneVerification,
	})
}
