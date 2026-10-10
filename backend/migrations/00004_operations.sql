-- +goose Up

-- Append-only audit trail of security-relevant and privileged actions.
CREATE TABLE audit_events (
    id          BIGSERIAL PRIMARY KEY,
    actor_id    UUID,
    actor_role  TEXT,
    action      TEXT NOT NULL,
    target_type TEXT,
    target_id   TEXT,
    details     JSONB NOT NULL DEFAULT '{}'::jsonb,
    ip          TEXT,
    request_id  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_actor_idx ON audit_events (actor_id, id DESC);
CREATE INDEX audit_events_target_idx ON audit_events (target_type, target_id, id DESC);
CREATE INDEX audit_events_action_idx ON audit_events (action, id DESC);
CREATE TRIGGER audit_events_immutable BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

CREATE TABLE notifications (
    id         UUID PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES users (id),
    category   TEXT NOT NULL CHECK (category IN ('transaction', 'security', 'account')),
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    data       JSONB NOT NULL DEFAULT '{}'::jsonb,
    dedupe_key TEXT UNIQUE,
    read_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX notifications_user_idx ON notifications (user_id, created_at DESC);

CREATE TABLE statements (
    id           UUID PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users (id),
    wallet_id    UUID NOT NULL REFERENCES wallets (id),
    period_start DATE NOT NULL,
    period_end   DATE NOT NULL CHECK (period_end >= period_start),
    format       TEXT NOT NULL CHECK (format IN ('csv', 'pdf')),
    content      BYTEA NOT NULL,
    sha256       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX statements_user_idx ON statements (user_id, created_at DESC);

-- Maker-checker workflow for privileged money movement (adjustments and
-- reversals). The approver must differ from the requester.
CREATE TABLE financial_requests (
    id            UUID PRIMARY KEY,
    kind          TEXT NOT NULL CHECK (kind IN ('adjustment_credit', 'adjustment_debit',
                                                'transfer_reversal', 'deposit_reversal', 'withdrawal_reversal')),
    wallet_id     UUID REFERENCES wallets (id),
    target_type   TEXT,
    target_id     UUID,
    amount        BIGINT CHECK (amount IS NULL OR amount > 0),
    currency      CHAR(3) REFERENCES currencies (code),
    reason_code   TEXT NOT NULL CHECK (reason_code IN ('provider_reversal', 'customer_dispute', 'operational_error',
                                                       'fraud', 'goodwill', 'reconciliation_correction')),
    memo          TEXT NOT NULL CHECK (char_length(memo) BETWEEN 10 AND 1000),
    status        TEXT NOT NULL DEFAULT 'pending_approval'
                  CHECK (status IN ('pending_approval', 'rejected', 'posted', 'failed')),
    requested_by  UUID NOT NULL REFERENCES users (id),
    decided_by    UUID REFERENCES users (id),
    decided_at    TIMESTAMPTZ,
    decision_note TEXT,
    journal_id    UUID REFERENCES journal_transactions (id),
    failure_reason TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT financial_requests_four_eyes CHECK (decided_by IS NULL OR decided_by <> requested_by),
    CONSTRAINT financial_requests_posted_has_journal CHECK (status <> 'posted' OR journal_id IS NOT NULL),
    CONSTRAINT financial_requests_adjustment_shape CHECK (
        kind NOT IN ('adjustment_credit', 'adjustment_debit')
        OR (wallet_id IS NOT NULL AND amount IS NOT NULL AND currency IS NOT NULL)),
    CONSTRAINT financial_requests_reversal_shape CHECK (
        kind IN ('adjustment_credit', 'adjustment_debit') OR (target_type IS NOT NULL AND target_id IS NOT NULL))
);
CREATE INDEX financial_requests_status_idx ON financial_requests (status, created_at DESC);
-- At most one open or posted reversal per target.
CREATE UNIQUE INDEX financial_requests_one_reversal_uidx ON financial_requests (target_type, target_id)
    WHERE target_id IS NOT NULL AND status IN ('pending_approval', 'posted');

CREATE TABLE reconciliation_runs (
    id          UUID PRIMARY KEY,
    source      TEXT NOT NULL,      -- 'internal', 'mpesa_statement', 'airtel_statement', ...
    status      TEXT NOT NULL CHECK (status IN ('running', 'completed', 'failed')),
    stats       JSONB NOT NULL DEFAULT '{}'::jsonb,
    started_by  UUID,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    error       TEXT
);

CREATE TABLE reconciliation_items (
    id               UUID PRIMARY KEY,
    run_id           UUID NOT NULL REFERENCES reconciliation_runs (id),
    discrepancy      TEXT NOT NULL CHECK (discrepancy IN (
                         'missing_internal', 'pending_but_completed', 'duplicate_provider_ref',
                         'amount_mismatch', 'currency_mismatch', 'success_without_posting',
                         'posting_without_evidence', 'uncertain_withdrawal', 'stale_pending',
                         'projection_drift', 'status_mismatch')),
    entity_type      TEXT,
    entity_id        TEXT,
    provider         TEXT,
    provider_ref     TEXT,
    details          JSONB NOT NULL DEFAULT '{}'::jsonb,
    fingerprint      TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'investigating', 'resolved', 'dismissed')),
    resolution_note  TEXT,
    resolved_by      UUID REFERENCES users (id),
    resolved_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The same unresolved discrepancy is reported only once across runs.
CREATE UNIQUE INDEX reconciliation_items_open_uidx ON reconciliation_items (fingerprint)
    WHERE status IN ('open', 'investigating');
CREATE INDEX reconciliation_items_status_idx ON reconciliation_items (status, created_at DESC);

-- +goose Down
DROP TABLE reconciliation_items;
DROP TABLE reconciliation_runs;
DROP TABLE financial_requests;
DROP TABLE statements;
DROP TABLE notifications;
DROP TABLE audit_events;
