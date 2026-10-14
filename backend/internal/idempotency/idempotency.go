// Package idempotency makes money-moving requests safe to retry.
//
// Protocol:
//  1. Acquire (own short transaction) claims (principal, operation, key) with a
//     request fingerprint. A completed key returns the original resource; a
//     different fingerprint is rejected; a live in-progress claim is rejected.
//  2. The service performs its business transaction and calls CompleteTx in
//     that same transaction, so "operation committed" and "key completed" are
//     atomic. A crash before commit leaves the key claimable again after its
//     lease expires, and nothing was committed.
//  3. If the operation is definitively rejected without side effects, Release
//     frees the key so the client may retry it.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sovereigntrust/wallet/internal/apperr"
)

// Retention is how long completed keys are kept. Clients must not reuse a
// key for a different request within this window.
const Retention = 7 * 24 * time.Hour

// Lease bounds how long an in-progress claim blocks concurrent duplicates.
const Lease = 60 * time.Second

type Key struct {
	Principal uuid.UUID
	Operation string
	Key       string
	Hash      []byte
	token     uuid.UUID
}

// Fingerprint hashes the canonical request parameters.
func Fingerprint(v any) []byte {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return h[:]
}

func ValidateKey(k string) error {
	if len(k) < 8 || len(k) > 128 {
		return apperr.New(apperr.CodeIdempotencyRequired, "Idempotency-Key header must be 8-128 characters")
	}
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return apperr.New(apperr.CodeIdempotencyRequired, "Idempotency-Key may contain only letters, digits, '-' and '_'")
		}
	}
	return nil
}

type Store struct{ Pool *pgxpool.Pool }

// Result is returned when the key was already completed.
type Result struct {
	ResourceType string
	ResourceID   uuid.UUID
}

// Acquire claims the key. If it returns a non-nil *Result the request is a
// replay and the caller must return that resource instead of re-executing.
func (s *Store) Acquire(ctx context.Context, k *Key) (*Result, error) {
	if err := ValidateKey(k.Key); err != nil {
		return nil, err
	}
	k.token = uuid.New()
	var res *Result
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (principal_id, operation, key, request_hash, status, owner_token, locked_until, expires_at)
			VALUES ($1, $2, $3, $4, 'in_progress', $5, now() + $6::interval, now() + $7::interval)
			ON CONFLICT DO NOTHING`,
			k.Principal, k.Operation, k.Key, k.Hash, k.token, pgInterval(Lease), pgInterval(Retention))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var hash []byte
		var status string
		var rtype *string
		var rid *uuid.UUID
		var lockExpired bool
		err = tx.QueryRow(ctx, `
			SELECT request_hash, status, resource_type, resource_id, locked_until < now()
			  FROM idempotency_keys WHERE principal_id = $1 AND operation = $2 AND key = $3
			   FOR UPDATE`, k.Principal, k.Operation, k.Key).Scan(&hash, &status, &rtype, &rid, &lockExpired)
		if err != nil {
			return err
		}
		if string(hash) != string(k.Hash) {
			return apperr.New(apperr.CodeIdempotencyMismatch, "this Idempotency-Key was already used with different request parameters")
		}
		if status == "completed" {
			if rtype == nil || rid == nil {
				return errors.New("completed idempotency key without resource")
			}
			res = &Result{ResourceType: *rtype, ResourceID: *rid}
			return nil
		}
		if !lockExpired {
			return apperr.New(apperr.CodeIdempotencyInFlight, "a request with this Idempotency-Key is still being processed")
		}
		// Previous holder died before committing anything; take over.
		_, err = tx.Exec(ctx, `
			UPDATE idempotency_keys SET owner_token = $4, locked_until = now() + $5::interval
			 WHERE principal_id = $1 AND operation = $2 AND key = $3`,
			k.Principal, k.Operation, k.Key, k.token, pgInterval(Lease))
		return err
	})
	return res, err
}

func pgInterval(d time.Duration) string { return fmt.Sprintf("%d seconds", int64(d.Seconds())) }

var errLostClaim = errors.New("idempotency claim lost to another request")

// CompleteTx marks the key completed inside the business transaction.
func (s *Store) CompleteTx(ctx context.Context, tx pgx.Tx, k *Key, resourceType string, resourceID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		   SET status = 'completed', resource_type = $5, resource_id = $6, completed_at = now()
		 WHERE principal_id = $1 AND operation = $2 AND key = $3 AND owner_token = $4 AND status = 'in_progress'`,
		k.Principal, k.Operation, k.Key, k.token, resourceType, resourceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errLostClaim
	}
	return nil
}

// Release frees a claim whose request was rejected without side effects.
func (s *Store) Release(ctx context.Context, k *Key) {
	_, _ = s.Pool.Exec(context.WithoutCancel(ctx), `
		DELETE FROM idempotency_keys
		 WHERE principal_id = $1 AND operation = $2 AND key = $3 AND owner_token = $4 AND status = 'in_progress'`,
		k.Principal, k.Operation, k.Key, k.token)
}

// Purge deletes expired keys.
func (s *Store) Purge(ctx context.Context) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)
	return tag.RowsAffected(), err
}
