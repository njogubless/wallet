// Package notify stores in-app notifications. Push/SMS delivery channels are
// not wired yet; notifications are delivered by the app polling the API.
package notify

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/db"
)

type Notification struct {
	ID        uuid.UUID       `json:"id"`
	Category  string          `json:"category"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Data      json.RawMessage `json:"data"`
	ReadAt    *time.Time      `json:"read_at"`
	CreatedAt time.Time       `json:"created_at"`
}

// Create inserts a notification, typically in the same transaction as the
// event. dedupeKey makes repeated processing harmless.
func Create(ctx context.Context, q db.Querier, userID uuid.UUID, category, title, body string, data map[string]any, dedupeKey string) error {
	if data == nil {
		data = map[string]any{}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var dk *string
	if dedupeKey != "" {
		dk = &dedupeKey
	}
	_, err = q.Exec(ctx, `
		INSERT INTO notifications (id, user_id, category, title, body, data, dedupe_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (dedupe_key) DO NOTHING`,
		uuid.Must(uuid.NewV7()), userID, category, title, body, b, dk)
	return err
}

func List(ctx context.Context, q db.Querier, userID uuid.UUID, before *time.Time, limit int) ([]Notification, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := q.Query(ctx, `
		SELECT id, category, title, body, data, read_at, created_at FROM notifications
		 WHERE user_id = $1 AND ($2::timestamptz IS NULL OR created_at < $2)
		 ORDER BY created_at DESC LIMIT $3`, userID, before, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Category, &n.Title, &n.Body, &n.Data, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var unread int
	err = q.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, userID).Scan(&unread)
	return out, unread, err
}

func MarkRead(ctx context.Context, q db.Querier, userID uuid.UUID, id *uuid.UUID) error {
	if id == nil {
		_, err := q.Exec(ctx, `UPDATE notifications SET read_at = now() WHERE user_id = $1 AND read_at IS NULL`, userID)
		return err
	}
	tag, err := q.Exec(ctx, `UPDATE notifications SET read_at = coalesce(read_at, now()) WHERE id = $1 AND user_id = $2`, *id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("notification")
	}
	return nil
}
