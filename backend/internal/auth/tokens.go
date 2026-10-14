package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type AccessClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

type TokenIssuer struct {
	Secret []byte
	Issuer string
	TTL    time.Duration
}

func (ti *TokenIssuer) Issue(userID uuid.UUID, role string, now time.Time) (string, time.Time, error) {
	exp := now.Add(ti.TTL)
	claims := AccessClaims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    ti.Issuer,
			Audience:  jwt.ClaimStrings{"wallet-api"},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        uuid.NewString(),
		},
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(ti.Secret)
	return s, exp, err
}

var ErrInvalidToken = errors.New("invalid access token")

func (ti *TokenIssuer) Verify(token string) (uuid.UUID, string, error) {
	var claims AccessClaims
	_, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		return ti.Secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(ti.Issuer),
		jwt.WithAudience("wallet-api"),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(5*time.Second),
	)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, "", ErrInvalidToken
	}
	return id, claims.Role, nil
}

// newOpaqueToken returns a 256-bit random token and its SHA-256 hash.
func newOpaqueToken() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	return tok, hashToken(tok), nil
}

func hashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}
