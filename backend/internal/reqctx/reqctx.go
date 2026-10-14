// Package reqctx carries request-scoped metadata through context.
package reqctx

import (
	"context"

	"github.com/google/uuid"
)

type Principal struct {
	UserID uuid.UUID
	Role   string
}

func (p Principal) IsStaff() bool { return p.Role == "admin" || p.Role == "support" }
func (p Principal) IsAdmin() bool { return p.Role == "admin" }

type Meta struct {
	RequestID string
	IP        string
	UserAgent string
}

type ctxKey int

const (
	principalKey ctxKey = iota
	metaKey
)

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

func WithMeta(ctx context.Context, m Meta) context.Context {
	return context.WithValue(ctx, metaKey, m)
}

func MetaFrom(ctx context.Context) Meta {
	m, _ := ctx.Value(metaKey).(Meta)
	return m
}
