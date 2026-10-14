// Package auth implements identity, sessions and transaction-PIN checks.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/audit"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/money"
	"github.com/sovereigntrust/wallet/internal/notify"
	"github.com/sovereigntrust/wallet/internal/phone"
	"github.com/sovereigntrust/wallet/internal/wallet"
)

// SMSSender delivers one-time codes.
type SMSSender interface {
	Send(ctx context.Context, to, message string) error
}

// LogSMS writes messages to the log. Development only (config refuses it in
// production): it exposes one-time codes to anyone who can read logs.
type LogSMS struct{ Log *slog.Logger }

func (l LogSMS) Send(_ context.Context, to, message string) error {
	l.Log.Warn("DEV SMS (not delivered)", "to", phone.Mask(to), "message", message)
	return nil
}

type DisabledSMS struct{}

func (DisabledSMS) Send(context.Context, string, string) error {
	return errors.New("SMS delivery is not configured")
}

const (
	maxLoginFailures = 5
	loginLockout     = 15 * time.Minute
	maxPINFailures   = 5
	pinLockout       = 30 * time.Minute
	otpTTL           = 10 * time.Minute
	maxOTPAttempts   = 5
)

type Service struct {
	Pool                     *pgxpool.Pool
	Tokens                   *TokenIssuer
	RefreshTTL               time.Duration
	SMS                      SMSSender
	OTPKey                   []byte
	RequirePhoneVerification bool
	Log                      *slog.Logger
}

type User struct {
	ID              uuid.UUID  `json:"id"`
	Phone           string     `json:"phone"`
	Email           *string    `json:"email"`
	FullName        string     `json:"full_name"`
	Role            string     `json:"role"`
	Status          string     `json:"status"`
	StatusReason    *string    `json:"status_reason,omitempty"`
	KYCTier         int        `json:"kyc_tier"`
	PhoneVerifiedAt *time.Time `json:"phone_verified_at"`
	EmailVerifiedAt *time.Time `json:"email_verified_at"`
	HasPIN          bool       `json:"has_pin"`
	CreatedAt       time.Time  `json:"created_at"`

	passwordHash     string
	pinHash          *string
	failedLogins     int
	loginLockedUntil *time.Time
	pinFailures      int
	pinLockedUntil   *time.Time
}

const userCols = `id, phone, email, full_name, role, status, status_reason, kyc_tier, phone_verified_at, email_verified_at,
	created_at, password_hash, pin_hash, failed_login_attempts, login_locked_until, pin_failed_attempts, pin_locked_until`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	var email *string
	err := row.Scan(&u.ID, &u.Phone, &email, &u.FullName, &u.Role, &u.Status, &u.StatusReason, &u.KYCTier,
		&u.PhoneVerifiedAt, &u.EmailVerifiedAt, &u.CreatedAt, &u.passwordHash, &u.pinHash, &u.failedLogins,
		&u.loginLockedUntil, &u.pinFailures, &u.pinLockedUntil)
	if err != nil {
		return nil, err
	}
	u.Email = email
	u.HasPIN = u.pinHash != nil
	return &u, nil
}

func GetUser(ctx context.Context, q db.Querier, id uuid.UUID) (*User, error) {
	u, err := scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("user")
	}
	return u, err
}

func lockUser(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*User, error) {
	u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("user")
	}
	return u, err
}

// CanMoveMoney reports whether the account may initiate outgoing payments.
func (u *User) CanSend(requireVerifiedPhone bool) error {
	if u.Status != "active" {
		return apperr.New(apperr.CodeAccountRestricted, "your account cannot send money (status: "+u.Status+")")
	}
	if requireVerifiedPhone && u.PhoneVerifiedAt == nil {
		return apperr.New(apperr.CodeAccountRestricted, "verify your phone number first")
	}
	return nil
}

// CanReceive reports whether funds may be credited (deposits / incoming transfers).
func (u *User) CanReceive() error {
	switch u.Status {
	case "active", "restricted", "pending_verification":
		return nil
	}
	return apperr.New(apperr.CodeAccountRestricted, "this account cannot receive funds")
}

// CanDeposit additionally requires verified phone when configured.
func (u *User) CanDeposit(requireVerifiedPhone bool) error {
	if u.Status != "active" && u.Status != "restricted" {
		return apperr.New(apperr.CodeAccountRestricted, "your account cannot make deposits (status: "+u.Status+")")
	}
	if requireVerifiedPhone && u.PhoneVerifiedAt == nil {
		return apperr.New(apperr.CodeAccountRestricted, "verify your phone number first")
	}
	return nil
}

type RegisterInput struct {
	Phone    string
	Email    string
	FullName string
	Password string
}

func validatePassword(pw string) error {
	if utf8.RuneCountInString(pw) < 10 {
		return apperr.Field("password", "must be at least 10 characters")
	}
	if len(pw) > 128 {
		return apperr.Field("password", "must be at most 128 bytes")
	}
	return nil
}

func (s *Service) Register(ctx context.Context, in RegisterInput) (*User, error) {
	fields := map[string]string{}
	ph, err := phone.NormalizeKE(in.Phone)
	if err != nil {
		fields["phone"] = "must be a valid Kenyan mobile number"
	}
	name := strings.TrimSpace(in.FullName)
	if n := utf8.RuneCountInString(name); n < 2 || n > 120 {
		fields["full_name"] = "must be 2-120 characters"
	}
	var email *string
	if e := strings.TrimSpace(in.Email); e != "" {
		if a, err := mail.ParseAddress(e); err != nil || a.Address != e || len(e) > 254 {
			fields["email"] = "must be a valid email address"
		} else {
			email = &e
		}
	}
	if err := validatePassword(in.Password); err != nil {
		e, _ := apperr.As(err)
		for k, v := range e.Fields {
			fields[k] = v
		}
	}
	if len(fields) > 0 {
		return nil, apperr.Validation("request validation failed", fields)
	}
	hash, err := HashSecret(in.Password)
	if err != nil {
		return nil, err
	}
	id := uuid.Must(uuid.NewV7())
	status := "active"
	if s.RequirePhoneVerification {
		status = "pending_verification"
	}
	err = db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO users (id, phone, email, full_name, password_hash, status)
			VALUES ($1, $2, $3, $4, $5, $6)`, id, ph, email, name, hash, status)
		if err != nil {
			if db.IsUniqueViolation(err, "") {
				// Same message for phone or email to limit enumeration.
				return apperr.New(apperr.CodeConflict, "an account with these details already exists")
			}
			return err
		}
		if _, err := wallet.Create(ctx, tx, id, money.KES.Code); err != nil {
			return err
		}
		return audit.RecordAs(ctx, tx, &id, ptr("user"), audit.Event{Action: "user.registered", TargetType: "user", TargetID: id.String()})
	})
	if err != nil {
		return nil, err
	}
	if s.RequirePhoneVerification {
		if err := s.sendCode(ctx, id, ph, "verify_phone"); err != nil {
			s.Log.Error("send verification code", "user_id", id, "err", err)
		}
	}
	return GetUser(ctx, s.Pool, id)
}

type Session struct {
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	TokenType        string    `json:"token_type"`
	User             *User     `json:"user"`
}

var errBadCredentials = apperr.New(apperr.CodeUnauthenticated, "invalid phone number or password")

func (s *Service) Login(ctx context.Context, phoneIn, password, userAgent, ip string) (*Session, error) {
	ph, err := phone.NormalizeKE(phoneIn)
	if err != nil {
		_, _ = VerifySecret(password, dummyHash)
		return nil, errBadCredentials
	}
	var sess *Session
	var outErr error
	err = db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE phone = $1 FOR UPDATE`, ph))
		if errors.Is(err, pgx.ErrNoRows) {
			_, _ = VerifySecret(password, dummyHash)
			outErr = errBadCredentials
			return nil
		}
		if err != nil {
			return err
		}
		now := time.Now()
		if u.loginLockedUntil != nil && u.loginLockedUntil.After(now) {
			outErr = apperr.New(apperr.CodeRateLimited, "too many failed sign-in attempts; try again later")
			return nil
		}
		ok, err := VerifySecret(password, u.passwordHash)
		if err != nil {
			return err
		}
		if !ok {
			failures := u.failedLogins + 1
			var lockUntil *time.Time
			if failures >= maxLoginFailures {
				t := now.Add(loginLockout)
				lockUntil = &t
				failures = 0
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET failed_login_attempts = $2, login_locked_until = $3, updated_at = now() WHERE id = $1`,
				u.ID, failures, lockUntil); err != nil {
				return err
			}
			if err := audit.RecordAs(ctx, tx, &u.ID, &u.Role, audit.Event{Action: "auth.login_failed", TargetType: "user", TargetID: u.ID.String(),
				Details: map[string]any{"locked": lockUntil != nil}}); err != nil {
				return err
			}
			outErr = errBadCredentials
			return nil
		}
		if u.Status == "suspended" || u.Status == "closed" {
			outErr = apperr.New(apperr.CodeAccountRestricted, "this account is "+u.Status+"; contact support")
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET failed_login_attempts = 0, login_locked_until = NULL, updated_at = now() WHERE id = $1`, u.ID); err != nil {
			return err
		}
		sess, err = s.newSession(ctx, tx, u, uuid.New(), userAgent, ip)
		if err != nil {
			return err
		}
		return audit.RecordAs(ctx, tx, &u.ID, &u.Role, audit.Event{Action: "auth.login", TargetType: "user", TargetID: u.ID.String()})
	})
	if err != nil {
		return nil, err
	}
	if outErr != nil {
		return nil, outErr
	}
	return sess, nil
}

func (s *Service) newSession(ctx context.Context, tx pgx.Tx, u *User, family uuid.UUID, userAgent, ip string) (*Session, error) {
	now := time.Now()
	access, accessExp, err := s.Tokens.Issue(u.ID, u.Role, now)
	if err != nil {
		return nil, err
	}
	refresh, hash, err := newOpaqueToken()
	if err != nil {
		return nil, err
	}
	refreshExp := now.Add(s.RefreshTTL)
	if len(userAgent) > 256 {
		userAgent = userAgent[:256]
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, uuid.Must(uuid.NewV7()), u.ID, family, hash, refreshExp, userAgent, ip)
	if err != nil {
		return nil, err
	}
	return &Session{AccessToken: access, AccessExpiresAt: accessExp, RefreshToken: refresh, RefreshExpiresAt: refreshExp,
		TokenType: "Bearer", User: u}, nil
}

var errBadRefresh = apperr.New(apperr.CodeUnauthenticated, "invalid or expired refresh token")

// Refresh rotates a refresh token. Presenting an already-rotated token is
// treated as theft: the whole token family is revoked.
func (s *Service) Refresh(ctx context.Context, token, userAgent, ip string) (*Session, error) {
	if token == "" || len(token) > 128 {
		return nil, errBadRefresh
	}
	var sess *Session
	var outErr error
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var id, userID, family uuid.UUID
		var expires time.Time
		var rotated, revoked *time.Time
		err := tx.QueryRow(ctx, `
			SELECT id, user_id, family_id, expires_at, rotated_at, revoked_at FROM refresh_tokens
			 WHERE token_hash = $1 FOR UPDATE`, hashToken(token)).Scan(&id, &userID, &family, &expires, &rotated, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			outErr = errBadRefresh
			return nil
		}
		if err != nil {
			return err
		}
		if rotated != nil && revoked == nil {
			if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL`, family); err != nil {
				return err
			}
			if err := audit.RecordAs(ctx, tx, &userID, nil, audit.Event{Action: "auth.refresh_token_reuse_detected",
				TargetType: "user", TargetID: userID.String(), Details: map[string]any{"family_id": family}}); err != nil {
				return err
			}
			if err := notify.Create(ctx, tx, userID, "security", "Session ended for your security",
				"We detected an old sign-in token being reused and signed you out. If this wasn't you, change your password.", nil, ""); err != nil {
				return err
			}
			outErr = errBadRefresh
			return nil
		}
		if revoked != nil || time.Now().After(expires) {
			outErr = errBadRefresh
			return nil
		}
		u, err := GetUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		if u.Status == "suspended" || u.Status == "closed" {
			outErr = apperr.New(apperr.CodeAccountRestricted, "this account is "+u.Status)
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET rotated_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		sess, err = s.newSession(ctx, tx, u, family, userAgent, ip)
		return err
	})
	if err != nil {
		return nil, err
	}
	if outErr != nil {
		return nil, outErr
	}
	return sess, nil
}

// Logout revokes the presented refresh token's family.
func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		 WHERE family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1) AND revoked_at IS NULL`, hashToken(token))
	return err
}

func (s *Service) revokeAll(ctx context.Context, q db.Querier, userID uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

// --- one-time codes ---

func (s *Service) codeHash(userID uuid.UUID, purpose, code string) []byte {
	m := hmac.New(sha256.New, s.OTPKey)
	m.Write([]byte(userID.String() + "|" + purpose + "|" + code))
	return m.Sum(nil)
}

func newOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func (s *Service) sendCode(ctx context.Context, userID uuid.UUID, to, purpose string) error {
	var recent int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM verification_codes WHERE user_id = $1 AND purpose = $2 AND created_at > now() - interval '1 hour'`,
		userID, purpose).Scan(&recent); err != nil {
		return err
	}
	if recent >= 5 {
		return apperr.New(apperr.CodeRateLimited, "too many codes requested; try again later")
	}
	code, err := newOTP()
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `
		INSERT INTO verification_codes (id, user_id, purpose, code_hash, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		uuid.Must(uuid.NewV7()), userID, purpose, s.codeHash(userID, purpose, code), time.Now().Add(otpTTL))
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("Your verification code is %s. It expires in 10 minutes. Never share it.", code)
	return s.SMS.Send(ctx, to, msg)
}

// consumeCode validates the latest unconsumed code for (user, purpose).
func (s *Service) consumeCode(ctx context.Context, tx pgx.Tx, userID uuid.UUID, purpose, code string) error {
	bad := apperr.Field("code", "invalid or expired code")
	var id uuid.UUID
	var hash []byte
	var attempts int
	var expires time.Time
	err := tx.QueryRow(ctx, `
		SELECT id, code_hash, attempts, expires_at FROM verification_codes
		 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL
		 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, userID, purpose).Scan(&id, &hash, &attempts, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return bad
	}
	if err != nil {
		return err
	}
	if attempts >= maxOTPAttempts || time.Now().After(expires) {
		return bad
	}
	if !hmac.Equal(hash, s.codeHash(userID, purpose, code)) {
		_, err := tx.Exec(ctx, `UPDATE verification_codes SET attempts = attempts + 1 WHERE id = $1`, id)
		if err != nil {
			return err
		}
		return errCodeMismatch
	}
	_, err = tx.Exec(ctx, `UPDATE verification_codes SET consumed_at = now() WHERE id = $1`, id)
	return err
}

var errCodeMismatch = apperr.Field("code", "invalid or expired code")

// withCodeTx runs fn after consuming a code; a mismatch still commits the
// attempt counter so brute force is bounded.
func (s *Service) withCodeTx(ctx context.Context, userID uuid.UUID, purpose, code string, fn func(pgx.Tx) error) error {
	var mismatch bool
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := s.consumeCode(ctx, tx, userID, purpose, code); err != nil {
			if errors.Is(err, errCodeMismatch) {
				mismatch = true
				return nil
			}
			return err
		}
		return fn(tx)
	})
	if err != nil {
		return err
	}
	if mismatch {
		return errCodeMismatch
	}
	return nil
}

func (s *Service) RequestPhoneVerification(ctx context.Context, userID uuid.UUID) error {
	u, err := GetUser(ctx, s.Pool, userID)
	if err != nil {
		return err
	}
	if u.PhoneVerifiedAt != nil {
		return apperr.New(apperr.CodeConflict, "phone number already verified")
	}
	return s.sendCode(ctx, userID, u.Phone, "verify_phone")
}

func (s *Service) VerifyPhone(ctx context.Context, userID uuid.UUID, code string) (*User, error) {
	err := s.withCodeTx(ctx, userID, "verify_phone", code, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE users SET phone_verified_at = coalesce(phone_verified_at, now()),
			       status = CASE WHEN status = 'pending_verification' THEN 'active' ELSE status END,
			       updated_at = now()
			 WHERE id = $1`, userID)
		if err != nil {
			return err
		}
		return audit.RecordAs(ctx, tx, &userID, ptr("user"), audit.Event{Action: "user.phone_verified", TargetType: "user", TargetID: userID.String()})
	})
	if err != nil {
		return nil, err
	}
	return GetUser(ctx, s.Pool, userID)
}

// ForgotPassword always succeeds from the caller's perspective to avoid
// revealing which numbers are registered.
func (s *Service) ForgotPassword(ctx context.Context, phoneIn string) error {
	ph, err := phone.NormalizeKE(phoneIn)
	if err != nil {
		return nil
	}
	var id uuid.UUID
	if err := s.Pool.QueryRow(ctx, `SELECT id FROM users WHERE phone = $1 AND status NOT IN ('closed')`, ph).Scan(&id); err != nil {
		return nil
	}
	if err := s.sendCode(ctx, id, ph, "reset_password"); err != nil {
		s.Log.Warn("password reset code not sent", "err", err)
	}
	return nil
}

func (s *Service) ResetPassword(ctx context.Context, phoneIn, code, newPassword string) error {
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	bad := apperr.Field("code", "invalid or expired code")
	ph, err := phone.NormalizeKE(phoneIn)
	if err != nil {
		return bad
	}
	var id uuid.UUID
	if err := s.Pool.QueryRow(ctx, `SELECT id FROM users WHERE phone = $1`, ph).Scan(&id); err != nil {
		return bad
	}
	hash, err := HashSecret(newPassword)
	if err != nil {
		return err
	}
	return s.withCodeTx(ctx, id, "reset_password", code, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, failed_login_attempts = 0, login_locked_until = NULL, updated_at = now() WHERE id = $1`, id, hash); err != nil {
			return err
		}
		if err := s.revokeAll(ctx, tx, id); err != nil {
			return err
		}
		if err := notify.Create(ctx, tx, id, "security", "Password changed", "Your password was reset. All sessions were signed out.", nil, ""); err != nil {
			return err
		}
		return audit.RecordAs(ctx, tx, &id, ptr("user"), audit.Event{Action: "auth.password_reset", TargetType: "user", TargetID: id.String()})
	})
}

func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, current, next string) error {
	if err := validatePassword(next); err != nil {
		return err
	}
	u, err := GetUser(ctx, s.Pool, userID)
	if err != nil {
		return err
	}
	ok, err := VerifySecret(current, u.passwordHash)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.Field("current_password", "incorrect password")
	}
	hash, err := HashSecret(next)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, userID, hash); err != nil {
			return err
		}
		if err := s.revokeAll(ctx, tx, userID); err != nil {
			return err
		}
		if err := notify.Create(ctx, tx, userID, "security", "Password changed", "Your password was changed and other sessions were signed out.", nil, ""); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "auth.password_changed", TargetType: "user", TargetID: userID.String()})
	})
}

// --- transaction PIN ---

func validatePIN(pin string) error {
	if len(pin) < 4 || len(pin) > 6 {
		return apperr.Field("pin", "must be 4-6 digits")
	}
	asc, desc, same := true, true, true
	for i := 0; i < len(pin); i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return apperr.Field("pin", "must contain digits only")
		}
		if i > 0 {
			asc = asc && pin[i] == pin[i-1]+1
			desc = desc && pin[i] == pin[i-1]-1
			same = same && pin[i] == pin[i-1]
		}
	}
	if asc || desc || same {
		return apperr.Field("pin", "is too easy to guess")
	}
	return nil
}

// SetPIN sets or changes the transaction PIN; the account password is required.
func (s *Service) SetPIN(ctx context.Context, userID uuid.UUID, password, pin string) error {
	if err := validatePIN(pin); err != nil {
		return err
	}
	u, err := GetUser(ctx, s.Pool, userID)
	if err != nil {
		return err
	}
	ok, err := VerifySecret(password, u.passwordHash)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.Field("password", "incorrect password")
	}
	hash, err := HashSecret(pin)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET pin_hash = $2, pin_failed_attempts = 0, pin_locked_until = NULL, updated_at = now() WHERE id = $1`, userID, hash); err != nil {
			return err
		}
		if err := notify.Create(ctx, tx, userID, "security", "Transaction PIN updated", "Your transaction PIN was set or changed.", nil, ""); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "auth.pin_set", TargetType: "user", TargetID: userID.String()})
	})
}

// VerifyPIN checks the transaction PIN in its own transaction so failed
// attempts are counted even when the payment is then rejected.
func (s *Service) VerifyPIN(ctx context.Context, userID uuid.UUID, pin string) error {
	var out error
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		u, err := lockUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		if u.pinHash == nil {
			out = apperr.New(apperr.CodePINNotSet, "set a transaction PIN first")
			return nil
		}
		now := time.Now()
		if u.pinLockedUntil != nil && u.pinLockedUntil.After(now) {
			out = apperr.New(apperr.CodePINLocked, "too many incorrect PIN attempts; try again later")
			return nil
		}
		ok, err := VerifySecret(pin, *u.pinHash)
		if err != nil {
			return err
		}
		if ok {
			if u.pinFailures > 0 {
				_, err = tx.Exec(ctx, `UPDATE users SET pin_failed_attempts = 0, pin_locked_until = NULL WHERE id = $1`, userID)
			}
			return err
		}
		failures := u.pinFailures + 1
		var lockUntil *time.Time
		if failures >= maxPINFailures {
			t := now.Add(pinLockout)
			lockUntil = &t
			failures = 0
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET pin_failed_attempts = $2, pin_locked_until = $3 WHERE id = $1`, userID, failures, lockUntil); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{Action: "auth.pin_failed", TargetType: "user", TargetID: userID.String(),
			Details: map[string]any{"locked": lockUntil != nil}}); err != nil {
			return err
		}
		if lockUntil != nil {
			if err := notify.Create(ctx, tx, userID, "security", "Transaction PIN locked",
				"Your PIN was entered incorrectly too many times and is locked for 30 minutes.", nil, ""); err != nil {
				return err
			}
			out = apperr.New(apperr.CodePINLocked, "too many incorrect PIN attempts; try again later")
		} else {
			out = apperr.New(apperr.CodeInvalidPIN, fmt.Sprintf("incorrect PIN; %d attempts remaining", maxPINFailures-failures))
		}
		return nil
	})
	if err != nil {
		return err
	}
	return out
}

func ptr[T any](v T) *T { return &v }
