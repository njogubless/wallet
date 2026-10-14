// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env         string // development | test | staging | production
	HTTPAddr    string
	MetricsAddr string
	DatabaseURL string
	RedisURL    string // optional; in-memory rate limiting is used when empty
	LogLevel    string

	JWTSecret  []byte
	JWTIssuer  string
	AccessTTL  time.Duration
	RefreshTTL time.Duration

	// RequirePhoneVerification gates money movement on a verified phone.
	RequirePhoneVerification bool
	// SMSMode: "log" (development only: codes are written to the server log)
	// or "disabled". No production SMS gateway is wired yet.
	SMSMode string

	// PublicBaseURL is the externally reachable HTTPS origin used to build
	// provider callback URLs, e.g. https://api.example.com
	PublicBaseURL string
	// CallbackToken is a high-entropy secret embedded in callback paths. It is
	// a defence-in-depth measure, not a substitute for status verification.
	CallbackToken string
	// CORSAllowedOrigins lists exact browser origins allowed to call the API
	// (e.g. the web admin console). Empty = no cross-origin access.
	CORSAllowedOrigins []string
	// TrustProxyHeaders enables X-Forwarded-For for client IPs (only behind a
	// trusted reverse proxy).
	TrustProxyHeaders bool

	// PaymentsMode: "simulated" (local deterministic provider, never moves real
	// money; refused in production) or "live" (uses the configured provider
	// environments below).
	PaymentsMode string

	DepositExpiry time.Duration

	Mpesa  MpesaConfig
	Airtel AirtelConfig
}

type MpesaConfig struct {
	Enabled         bool
	Environment     string // sandbox | production
	BaseURL         string
	ConsumerKey     string
	ConsumerSecret  string
	ShortCode       string // STK push business short code (paybill or till)
	PartyB          string // receiving party; defaults to ShortCode (differs for till numbers)
	Passkey         string
	TransactionType string // CustomerPayBillOnline | CustomerBuyGoodsOnline
	// B2C (payouts)
	B2CShortCode      string
	InitiatorName     string
	InitiatorPassword string
	CertificatePath   string // Safaricom public certificate (PEM) for SecurityCredential
	B2CCommandID      string // BusinessPayment | SalaryPayment | PromotionPayment
	// Optional allowlist of source IPs/CIDRs for callbacks.
	CallbackAllowlist []string
}

type AirtelConfig struct {
	Enabled      bool
	Environment  string // sandbox | production
	BaseURL      string
	ClientID     string
	ClientSecret string
	Country      string
	Currency     string
	// Disbursement PIN, RSA-encrypted with Airtel's public key at call time.
	DisbursementPIN   string
	PublicKeyPEM      string // Airtel-provided RSA public key (PEM or base64 DER)
	CallbackAllowlist []string
}

func (c *Config) IsProduction() bool { return c.Env == "production" }

func Load() (*Config, error) {
	c := &Config{
		Env:                      get("APP_ENV", "development"),
		HTTPAddr:                 get("HTTP_ADDR", ":8080"),
		MetricsAddr:              get("METRICS_ADDR", ":9090"),
		DatabaseURL:              os.Getenv("DATABASE_URL"),
		RedisURL:                 os.Getenv("REDIS_URL"),
		LogLevel:                 get("LOG_LEVEL", "info"),
		JWTSecret:                []byte(os.Getenv("JWT_SECRET")),
		JWTIssuer:                get("JWT_ISSUER", "wallet-api"),
		AccessTTL:                getDuration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTTL:               getDuration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
		RequirePhoneVerification: getBool("REQUIRE_PHONE_VERIFICATION", true),
		SMSMode:                  get("SMS_MODE", "log"),
		PublicBaseURL:            strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"),
		CallbackToken:            os.Getenv("CALLBACK_TOKEN"),
		TrustProxyHeaders:        getBool("TRUST_PROXY_HEADERS", false),
		CORSAllowedOrigins:       getList("CORS_ALLOWED_ORIGINS"),
		PaymentsMode:             get("PAYMENTS_MODE", "simulated"),
		DepositExpiry:            getDuration("DEPOSIT_EXPIRY", 3*time.Minute),
		Mpesa: MpesaConfig{
			Enabled:           getBool("MPESA_ENABLED", false),
			Environment:       get("MPESA_ENVIRONMENT", "sandbox"),
			BaseURL:           os.Getenv("MPESA_BASE_URL"),
			ConsumerKey:       os.Getenv("MPESA_CONSUMER_KEY"),
			ConsumerSecret:    os.Getenv("MPESA_CONSUMER_SECRET"),
			ShortCode:         os.Getenv("MPESA_SHORTCODE"),
			PartyB:            os.Getenv("MPESA_PARTY_B"),
			Passkey:           os.Getenv("MPESA_PASSKEY"),
			TransactionType:   get("MPESA_TRANSACTION_TYPE", "CustomerPayBillOnline"),
			B2CShortCode:      os.Getenv("MPESA_B2C_SHORTCODE"),
			InitiatorName:     os.Getenv("MPESA_INITIATOR_NAME"),
			InitiatorPassword: os.Getenv("MPESA_INITIATOR_PASSWORD"),
			CertificatePath:   os.Getenv("MPESA_CERT_PATH"),
			B2CCommandID:      get("MPESA_B2C_COMMAND_ID", "BusinessPayment"),
			CallbackAllowlist: getList("MPESA_CALLBACK_ALLOWLIST"),
		},
		Airtel: AirtelConfig{
			Enabled:           getBool("AIRTEL_ENABLED", false),
			Environment:       get("AIRTEL_ENVIRONMENT", "sandbox"),
			BaseURL:           os.Getenv("AIRTEL_BASE_URL"),
			ClientID:          os.Getenv("AIRTEL_CLIENT_ID"),
			ClientSecret:      os.Getenv("AIRTEL_CLIENT_SECRET"),
			Country:           get("AIRTEL_COUNTRY", "KE"),
			Currency:          get("AIRTEL_CURRENCY", "KES"),
			DisbursementPIN:   os.Getenv("AIRTEL_DISBURSEMENT_PIN"),
			PublicKeyPEM:      os.Getenv("AIRTEL_PUBLIC_KEY"),
			CallbackAllowlist: getList("AIRTEL_CALLBACK_ALLOWLIST"),
		},
	}
	if c.Mpesa.BaseURL == "" {
		c.Mpesa.BaseURL = map[string]string{
			"sandbox":    "https://sandbox.safaricom.co.ke",
			"production": "https://api.safaricom.co.ke",
		}[c.Mpesa.Environment]
	}
	if c.Mpesa.PartyB == "" {
		c.Mpesa.PartyB = c.Mpesa.ShortCode
	}
	if c.Airtel.BaseURL == "" {
		c.Airtel.BaseURL = map[string]string{
			"sandbox":    "https://openapiuat.airtel.africa",
			"production": "https://openapi.airtel.africa",
		}[c.Airtel.Environment]
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if len(c.JWTSecret) < 32 {
		errs = append(errs, errors.New("JWT_SECRET must be at least 32 bytes"))
	}
	switch c.PaymentsMode {
	case "simulated":
		if c.IsProduction() {
			errs = append(errs, errors.New("PAYMENTS_MODE=simulated is not allowed in production"))
		}
	case "live":
		if c.PublicBaseURL == "" || !strings.HasPrefix(c.PublicBaseURL, "https://") {
			errs = append(errs, errors.New("PUBLIC_BASE_URL must be an https URL in live payments mode"))
		}
		if len(c.CallbackToken) < 24 {
			errs = append(errs, errors.New("CALLBACK_TOKEN must be at least 24 characters in live payments mode"))
		}
		if c.Mpesa.Enabled {
			if c.Mpesa.BaseURL == "" {
				errs = append(errs, fmt.Errorf("unknown MPESA_ENVIRONMENT %q", c.Mpesa.Environment))
			}
			for k, v := range map[string]string{
				"MPESA_CONSUMER_KEY": c.Mpesa.ConsumerKey, "MPESA_CONSUMER_SECRET": c.Mpesa.ConsumerSecret,
				"MPESA_SHORTCODE": c.Mpesa.ShortCode, "MPESA_PASSKEY": c.Mpesa.Passkey,
			} {
				if v == "" {
					errs = append(errs, fmt.Errorf("%s is required when MPESA_ENABLED=true", k))
				}
			}
		}
		if c.Airtel.Enabled {
			if c.Airtel.BaseURL == "" {
				errs = append(errs, fmt.Errorf("unknown AIRTEL_ENVIRONMENT %q", c.Airtel.Environment))
			}
			if c.Airtel.ClientID == "" || c.Airtel.ClientSecret == "" {
				errs = append(errs, errors.New("AIRTEL_CLIENT_ID and AIRTEL_CLIENT_SECRET are required when AIRTEL_ENABLED=true"))
			}
		}
	default:
		errs = append(errs, fmt.Errorf("PAYMENTS_MODE must be simulated or live, got %q", c.PaymentsMode))
	}
	if c.SMSMode == "log" && c.IsProduction() {
		errs = append(errs, errors.New("SMS_MODE=log is not allowed in production"))
	}
	return errors.Join(errs...)
}

func get(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func getBool(k string, def bool) bool {
	v, err := strconv.ParseBool(os.Getenv(k))
	if err != nil {
		return def
	}
	return v
}

func getDuration(k string, def time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(k))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func getList(k string) []string {
	var out []string
	for _, p := range strings.Split(os.Getenv(k), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
