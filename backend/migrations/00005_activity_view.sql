-- +goose Up

-- Customer-facing activity feed: one row per business operation per wallet.
-- Amounts are positive; `direction` says whether it adds to or removes from
-- the wallet. Authoritative balances still come from the ledger.
CREATE VIEW wallet_activity AS
SELECT d.wallet_id, d.user_id, d.id, 'deposit'::text AS type, 'credit'::text AS direction,
       d.amount, 0::bigint AS fee, d.currency, d.status, d.provider,
       NULL::text AS counterparty, d.msisdn AS counterparty_detail,
       d.provider_receipt AS reference, ''::text AS description,
       d.failure_reason, d.created_at, d.completed_at
  FROM deposits d
UNION ALL
SELECT w.wallet_id, w.user_id, w.id, 'withdrawal', 'debit',
       w.amount, w.fee, w.currency, w.status, w.provider,
       NULL, w.msisdn, w.provider_receipt, '', w.failure_reason, w.created_at, w.completed_at
  FROM withdrawals w
UNION ALL
SELECT t.sender_wallet_id, t.sender_user_id, t.id, 'transfer_out', 'debit',
       t.amount, t.fee, t.currency, t.status, NULL,
       ru.full_name, rw.account_number, NULL, t.description, t.failure_reason, t.created_at, t.completed_at
  FROM transfers t
  JOIN users ru ON ru.id = t.recipient_user_id
  JOIN wallets rw ON rw.id = t.recipient_wallet_id
UNION ALL
SELECT t.recipient_wallet_id, t.recipient_user_id, t.id, 'transfer_in', 'credit',
       t.amount, 0, t.currency, t.status, NULL,
       su.full_name, sw.account_number, NULL, t.description, NULL, t.created_at, t.completed_at
  FROM transfers t
  JOIN users su ON su.id = t.sender_user_id
  JOIN wallets sw ON sw.id = t.sender_wallet_id
 WHERE t.status IN ('succeeded', 'reversed')
UNION ALL
SELECT f.wallet_id, w.user_id, f.id, 'adjustment',
       CASE WHEN f.kind = 'adjustment_credit' THEN 'credit' ELSE 'debit' END,
       f.amount, 0, f.currency, 'succeeded', NULL,
       NULL, NULL, NULL, f.reason_code, NULL, f.created_at, f.decided_at
  FROM financial_requests f
  JOIN wallets w ON w.id = f.wallet_id
 WHERE f.kind IN ('adjustment_credit', 'adjustment_debit') AND f.status = 'posted';

-- +goose Down
DROP VIEW wallet_activity;
