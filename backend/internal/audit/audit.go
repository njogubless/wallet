// Package audit writes append-only audit events.
package audit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/reqctx"
)

type Event struct {
	Action     string
	TargetType string
	TargetID   string
	Details    map[string]any
}

// Record writes an audit event using the principal and request metadata from
// ctx. Pass a transaction as q to make the event atomic with the action.
func Record(ctx context.Context, q db.Querier, e Event) error {
	var actorID *uuid.UUID
	var role *string
	if p, ok := reqctx.PrincipalFrom(ctx); ok {
		actorID, role = &p.UserID, &p.Role
	}
	return RecordAs(ctx, q, actorID, role, e)
}

// RecordAs writes an audit event for an explicit actor (nil for system).
func RecordAs(ctx context.Context, q db.Querier, actorID *uuid.UUID, role *string, e Event) error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	details, err := json.Marshal(e.Details)
	if err != nil {
		return err
	}
	m := reqctx.MetaFrom(ctx)
	_, err = q.Exec(ctx, `
		INSERT INTO audit_events (actor_id, actor_role, action, target_type, target_id, details, ip, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		actorID, role, e.Action, nullable(e.TargetType), nullable(e.TargetID), details, nullable(m.IP), nullable(m.RequestID))
	return err
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type Row struct {
	ID         int64           `json:"id"`
	ActorID    *uuid.UUID      `json:"actor_id"`
	ActorRole  *string         `json:"actor_role"`
	Action     string          `json:"action"`
	TargetType *string         `json:"target_type"`
	TargetID   *string         `json:"target_id"`
	Details    json.RawMessage `json:"details"`
	IP         *string         `json:"ip"`
	RequestID  *string         `json:"request_id"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Filter struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	BeforeID   int64
	Limit      int
}

func List(ctx context.Context, q db.Querier, f Filter) ([]Row, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := q.Query(ctx, `
		SELECT id, actor_id, actor_role, action, target_type, target_id, details, ip, request_id, created_at
		  FROM audit_events
		 WHERE ($1::uuid IS NULL OR actor_id = $1)
		   AND ($2 = '' OR action = $2)
		   AND ($3 = '' OR target_type = $3)
		   AND ($4 = '' OR target_id = $4)
		   AND ($5 = 0 OR id < $5)
		 ORDER BY id DESC LIMIT $6`,
		f.ActorID, f.Action, f.TargetType, f.TargetID, f.BeforeID, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.ActorID, &r.ActorRole, &r.Action, &r.TargetType, &r.TargetID, &r.Details, &r.IP, &r.RequestID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
