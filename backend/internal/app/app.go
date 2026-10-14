// Package app wires configuration into services; shared by all binaries.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/sovereigntrust/wallet/internal/admin"
	"github.com/sovereigntrust/wallet/internal/auth"
	"github.com/sovereigntrust/wallet/internal/callbacks"
	"github.com/sovereigntrust/wallet/internal/config"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/deposits"
	"github.com/sovereigntrust/wallet/internal/idempotency"
	"github.com/sovereigntrust/wallet/internal/payments"
	"github.com/sovereigntrust/wallet/internal/payments/airtel"
	"github.com/sovereigntrust/wallet/internal/payments/mpesa"
	"github.com/sovereigntrust/wallet/internal/payments/simulator"
	"github.com/sovereigntrust/wallet/internal/reconciliation"
	"github.com/sovereigntrust/wallet/internal/statements"
	"github.com/sovereigntrust/wallet/internal/transfers"
	"github.com/sovereigntrust/wallet/internal/withdrawals"
)

type App struct {
	Cfg            *config.Config
	Log            *slog.Logger
	Pool           *pgxpool.Pool
	Redis          *redis.Client
	Tokens         *auth.TokenIssuer
	Auth           *auth.Service
	Idem           *idempotency.Store
	Providers      payments.Registry
	Transfers      *transfers.Service
	Deposits       *deposits.Service
	Withdrawals    *withdrawals.Service
	Callbacks      *callbacks.Store
	Processor      *callbacks.Processor
	Statements     *statements.Service
	Admin          *admin.Service
	Reconciliation *reconciliation.Service
}

func NewLogger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

func buildProviders(cfg *config.Config, log *slog.Logger) (payments.Registry, error) {
	reg := payments.Registry{}
	if cfg.PaymentsMode == "simulated" {
		log.Warn("PAYMENTS_MODE=simulated: no real provider is contacted and no real money moves")
		reg["mpesa"] = simulator.New("mpesa")
		reg["airtel"] = simulator.New("airtel")
		return reg, nil
	}
	hc := payments.NewHTTPClient()
	if cfg.Mpesa.Enabled {
		c, err := mpesa.New(cfg.Mpesa, hc)
		if err != nil {
			return nil, err
		}
		reg["mpesa"] = c
	}
	if cfg.Airtel.Enabled {
		c, err := airtel.New(cfg.Airtel, hc)
		if err != nil {
			return nil, err
		}
		reg["airtel"] = c
	}
	return reg, nil
}

func New(ctx context.Context, cfg *config.Config) (*App, error) {
	log := NewLogger(cfg.LogLevel).With("env", cfg.Env)
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	var rdb *redis.Client
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("REDIS_URL: %w", err)
		}
		rdb = redis.NewClient(opt)
	}
	providers, err := buildProviders(cfg, log)
	if err != nil {
		return nil, err
	}
	var sms auth.SMSSender = auth.DisabledSMS{}
	if cfg.SMSMode == "log" {
		sms = auth.LogSMS{Log: log}
	}
	tokens := &auth.TokenIssuer{Secret: cfg.JWTSecret, Issuer: cfg.JWTIssuer, TTL: cfg.AccessTTL}
	authSvc := &auth.Service{Pool: pool, Tokens: tokens, RefreshTTL: cfg.RefreshTTL, SMS: sms, OTPKey: cfg.JWTSecret,
		RequirePhoneVerification: cfg.RequirePhoneVerification, Log: log}
	idem := &idempotency.Store{Pool: pool}
	cbBase := func(provider string) string {
		if cfg.PaymentsMode != "live" {
			return ""
		}
		return cfg.PublicBaseURL + "/api/v1/callbacks/" + provider + "/" + cfg.CallbackToken
	}
	dep := &deposits.Service{Pool: pool, Providers: providers, Idem: idem, RequirePhoneVerification: cfg.RequirePhoneVerification,
		Expiry: cfg.DepositExpiry, Log: log, CallbackURL: func(p string) string {
			base := cbBase(p)
			if base == "" || p != "mpesa" {
				return base // Airtel callbacks are configured in the Airtel portal, not per request
			}
			return base + "/stk"
		}}
	wd := &withdrawals.Service{Pool: pool, Providers: providers, Idem: idem, Auth: authSvc, RequirePhoneVerification: cfg.RequirePhoneVerification,
		Log: log, ResultURLs: func(p string) (string, string) {
			base := cbBase(p)
			if base == "" || p != "mpesa" {
				return base, base
			}
			return base + "/b2c/result", base + "/b2c/timeout"
		}}
	return &App{
		Cfg: cfg, Log: log, Pool: pool, Redis: rdb, Tokens: tokens, Auth: authSvc, Idem: idem, Providers: providers,
		Transfers: &transfers.Service{Pool: pool, Auth: authSvc, Idem: idem, RequirePhoneVerification: cfg.RequirePhoneVerification},
		Deposits:  dep, Withdrawals: wd,
		Callbacks:      &callbacks.Store{Pool: pool},
		Processor:      &callbacks.Processor{Pool: pool, Deposits: dep, Withdrawals: wd, Log: log},
		Statements:     &statements.Service{Pool: pool},
		Admin:          &admin.Service{Pool: pool},
		Reconciliation: &reconciliation.Service{Pool: pool, Log: log},
	}, nil
}

func (a *App) Close() {
	if a.Redis != nil {
		_ = a.Redis.Close()
	}
	a.Pool.Close()
}

// RedactURL hides credentials in a connection URL for logging.
func RedactURL(u string) string {
	if i := strings.Index(u, "@"); i > 0 {
		if j := strings.Index(u, "://"); j > 0 && j < i {
			return u[:j+3] + "***" + u[i:]
		}
	}
	return u
}
