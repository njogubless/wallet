// Package apperr defines the stable, client-facing error vocabulary.
// Handlers map these to HTTP responses; anything else becomes a generic 500.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

type Code string

const (
	CodeValidation          Code = "validation_error"
	CodeUnauthenticated     Code = "unauthenticated"
	CodeForbidden           Code = "forbidden"
	CodeNotFound            Code = "not_found"
	CodeConflict            Code = "conflict"
	CodeInsufficientFunds   Code = "insufficient_funds"
	CodeLimitExceeded       Code = "limit_exceeded"
	CodeAccountRestricted   Code = "account_restricted"
	CodeIdempotencyMismatch Code = "idempotency_key_reused"
	CodeIdempotencyInFlight Code = "idempotency_request_in_progress"
	CodeIdempotencyRequired Code = "idempotency_key_required"
	CodeInvalidPIN          Code = "invalid_pin"
	CodePINLocked           Code = "pin_locked"
	CodePINNotSet           Code = "pin_not_set"
	CodeRateLimited         Code = "rate_limited"
	CodeProviderUnavailable Code = "provider_unavailable"
	CodeInvalidState        Code = "invalid_state"
	CodeInternal            Code = "internal_error"
)

var statusByCode = map[Code]int{
	CodeValidation:          http.StatusUnprocessableEntity,
	CodeUnauthenticated:     http.StatusUnauthorized,
	CodeForbidden:           http.StatusForbidden,
	CodeNotFound:            http.StatusNotFound,
	CodeConflict:            http.StatusConflict,
	CodeInsufficientFunds:   http.StatusUnprocessableEntity,
	CodeLimitExceeded:       http.StatusUnprocessableEntity,
	CodeAccountRestricted:   http.StatusForbidden,
	CodeIdempotencyMismatch: http.StatusUnprocessableEntity,
	CodeIdempotencyInFlight: http.StatusConflict,
	CodeIdempotencyRequired: http.StatusBadRequest,
	CodeInvalidPIN:          http.StatusUnauthorized,
	CodePINLocked:           http.StatusLocked,
	CodePINNotSet:           http.StatusPreconditionRequired,
	CodeRateLimited:         http.StatusTooManyRequests,
	CodeProviderUnavailable: http.StatusServiceUnavailable,
	CodeInvalidState:        http.StatusConflict,
	CodeInternal:            http.StatusInternalServerError,
}

// Error is a domain error safe to show to clients.
type Error struct {
	Code    Code
	Message string
	Fields  map[string]string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func (e *Error) HTTPStatus() int {
	if s, ok := statusByCode[e.Code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

func New(code Code, msg string) *Error { return &Error{Code: code, Message: msg} }

func Validation(msg string, fields map[string]string) *Error {
	return &Error{Code: CodeValidation, Message: msg, Fields: fields}
}

func Field(field, msg string) *Error {
	return Validation("request validation failed", map[string]string{field: msg})
}

func NotFound(what string) *Error { return New(CodeNotFound, what+" not found") }

var (
	ErrUnauthenticated   = New(CodeUnauthenticated, "authentication required")
	ErrForbidden         = New(CodeForbidden, "you are not allowed to perform this action")
	ErrInsufficientFunds = New(CodeInsufficientFunds, "insufficient available balance")
)

// As extracts an *Error from err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Is reports whether err carries the given code.
func Is(err error, code Code) bool {
	e, ok := As(err)
	return ok && e.Code == code
}
