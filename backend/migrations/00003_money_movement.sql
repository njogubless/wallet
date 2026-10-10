-- +goose Up

-- Idempotency records for money-moving requests, scoped to principal + operation.
CREATE TABLE idempotency_keys (
    principal_id    UUID NOT NULL,
    operation       TEXT NOT NULL,
    key             TEXT NOT NULL CHECK (char_length(key) BETWEEN 8 AND 128),
    request_hash    BYTEA NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('in_progress', 'completed')),
    owner_token     UUID NOT NULL,          -- identifies the request currently holding the key
    resource_type   TEXT,                   -- resource created by the completed request
    resource_id     UUID,
    locked_until    TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (principal_id, operation, key)
);
CREATE INDEX idempotency_keys_expiry_idx ON idempotency_keys (expires_at);

-- Fee and limit configuration is versioned by insertion; the rule in force is
-- the latest one whose effective_from has passed. Rows are never edited.
CREATE TABLE fee_rules (
    id             UUID PRIMARY KEY,
    operation      TEXT NOT NULL CHECK (operation IN ('deposit', 'transfer', 'withdrawal')),
    provider       TEXT NOT NULL DEFAULT '*',
    currency       CHAR(3) NOT NULL REFERENCES currencies (code),
    flat_fee       BIGINT NOT NULL DEFAULT 0 CHECK (flat_fee >= 0),
    rate_bps       INT NOT NULL DEFAULT 0 CHECK (rate_bps BETWEEN 0 AND 10000),
    min_fee        BIGINT NOT NULL DEFAULT 0 CHECK (min_fee >= 0),
    max_fee        BIGINT CHECK (max_fee IS NULL OR max_fee >= min_fee),
    effective_from TIMESTAMPTZ NOT NULL DEFAULT now(),
    reason         TEXT NOT NULL,
    created_by     UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX fee_rules_lookup_idx ON fee_rules (operation, provider, currency, effective_from DESC);

CREATE TABLE limit_rules (
    id             UUID PRIMARY KEY,
    kyc_tier       SMALLINT NOT NULL CHECK (kyc_tier BETWEEN 0 AND 3),
    operation      TEXT NOT NULL CHECK (operation IN ('deposit', 'transfer', 'withdrawal')),
    currency       CHAR(3) NOT NULL REFERENCES currencies (code),
    per_txn_min    BIGINT NOT NULL CHECK (per_txn_min > 0),
    per_txn_max    BIGINT NOT NULL CHECK (per_txn_max >= per_txn_min),
    daily_max      BIGINT NOT NULL CHECK (daily_max >= per_txn_max),
    effective_from TIMESTAMPTZ NOT NULL DEFAULT now(),
    reason         TEXT NOT NULL,
    created_by     UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX limit_rules_lookup_idx ON limit_rules (kyc_tier, operation, currency, effective_from DESC);

-- Development defaults. These are placeholders, NOT regulatory values; replace
-- them through the admin settings workflow before going live.
INSERT INTO fee_rules (id, operation, provider, currency, flat_fee, rate_bps, min_fee, max_fee, reason) VALUES
    ('00000000-0000-0000-0000-00000000f001', 'deposit',    '*', 'KES', 0,    0, 0, NULL, 'initial default'),
    ('00000000-0000-0000-0000-00000000f002', 'transfer',   '*', 'KES', 0,    0, 0, NULL, 'initial default'),
    ('00000000-0000-0000-0000-00000000f003', 'withdrawal', '*', 'KES', 1500, 50, 1500, 30000, 'initial default');
INSERT INTO limit_rules (id, kyc_tier, operation, currency, per_txn_min, per_txn_max, daily_max, reason)
SELECT gen_random_uuid(), t.tier, o.op, 'KES', 1000, t.per_txn, t.daily, 'initial default'
  FROM (VALUES (1, 7000000::BIGINT, 15000000::BIGINT),
               (2, 15000000::BIGINT, 30000000::BIGINT),
               (3, 25000000::BIGINT, 50000000::BIGINT)) AS t(tier, per_txn, daily)
 CROSS JOIN (VALUES ('deposit'), ('transfer'), ('withdrawal')) AS o(op);

CREATE TABLE deposits (
    id                    UUID PRIMARY KEY,
    user_id               UUID NOT NULL REFERENCES users (id),
    wallet_id             UUID NOT NULL REFERENCES wallets (id),
    provider              TEXT NOT NULL CHECK (provider IN ('mpesa', 'airtel')),
    provider_env          TEXT NOT NULL CHECK (provider_env IN ('simulated', 'sandbox', 'production')),
    msisdn                TEXT NOT NULL,
    amount                BIGINT NOT NULL CHECK (amount > 0),
    currency              CHAR(3) NOT NULL REFERENCES currencies (code),
    status                TEXT NOT NULL CHECK (status IN ('created', 'pending_provider', 'succeeded', 'failed', 'expired', 'reversed')),
    provider_status       TEXT NOT NULL DEFAULT 'not_sent'
                          CHECK (provider_status IN ('not_sent', 'sending', 'accepted', 'rejected', 'unknown',
                                                     'completed', 'failed')),
    provider_request_ref  TEXT,     -- reference we generate and send (Airtel transaction.id)
    provider_checkout_ref TEXT,     -- provider-issued request handle (Daraja CheckoutRequestID)
    provider_merchant_ref TEXT,     -- Daraja MerchantRequestID
    provider_receipt      TEXT,     -- final provider transaction id (MpesaReceiptNumber / airtel_money_id)
    provider_result_code  TEXT,
    provider_result_desc  TEXT,
    failure_reason        TEXT,
    journal_id            UUID REFERENCES journal_transactions (id),
    reversal_journal_id   UUID REFERENCES journal_transactions (id),
    needs_review          BOOLEAN NOT NULL DEFAULT false,
    expires_at            TIMESTAMPTZ NOT NULL,
    dispatched_at         TIMESTAMPTZ,
    completed_at          TIMESTAMPTZ,
    last_polled_at        TIMESTAMPTZ,
    poll_count            INT NOT NULL DEFAULT 0,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT deposits_success_has_journal CHECK (status NOT IN ('succeeded', 'reversed') OR journal_id IS NOT NULL),
    CONSTRAINT deposits_reversed_has_journal CHECK (status <> 'reversed' OR reversal_journal_id IS NOT NULL),
    CONSTRAINT deposits_journal_only_on_success CHECK (journal_id IS NULL OR status IN ('succeeded', 'reversed'))
);
CREATE UNIQUE INDEX deposits_request_ref_uidx  ON deposits (provider, provider_request_ref)  WHERE provider_request_ref IS NOT NULL;
CREATE UNIQUE INDEX deposits_checkout_ref_uidx ON deposits (provider, provider_checkout_ref) WHERE provider_checkout_ref IS NOT NULL;
CREATE UNIQUE INDEX deposits_receipt_uidx      ON deposits (provider, provider_receipt)      WHERE provider_receipt IS NOT NULL;
CREATE INDEX deposits_user_idx ON deposits (user_id, created_at DESC);
CREATE INDEX deposits_open_idx ON deposits (status, updated_at) WHERE status IN ('created', 'pending_provider');

CREATE TABLE transfers (
    id                  UUID PRIMARY KEY,
    sender_user_id      UUID NOT NULL REFERENCES users (id),
    sender_wallet_id    UUID NOT NULL REFERENCES wallets (id),
    recipient_user_id   UUID NOT NULL REFERENCES users (id),
    recipient_wallet_id UUID NOT NULL REFERENCES wallets (id),
    amount              BIGINT NOT NULL CHECK (amount > 0),
    fee                 BIGINT NOT NULL DEFAULT 0 CHECK (fee >= 0),
    currency            CHAR(3) NOT NULL REFERENCES currencies (code),
    status              TEXT NOT NULL CHECK (status IN ('created', 'processing', 'succeeded', 'failed', 'reversed')),
    description         TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 140),
    failure_reason      TEXT,
    journal_id          UUID REFERENCES journal_transactions (id),
    reversal_journal_id UUID REFERENCES journal_transactions (id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at        TIMESTAMPTZ,
    CONSTRAINT transfers_distinct_wallets CHECK (sender_wallet_id <> recipient_wallet_id),
    CONSTRAINT transfers_success_has_journal CHECK (status NOT IN ('succeeded', 'reversed') OR journal_id IS NOT NULL),
    CONSTRAINT transfers_reversed_has_journal CHECK (status <> 'reversed' OR reversal_journal_id IS NOT NULL)
);
CREATE INDEX transfers_sender_idx ON transfers (sender_wallet_id, created_at DESC);
CREATE INDEX transfers_recipient_idx ON transfers (recipient_wallet_id, created_at DESC);

CREATE TABLE withdrawals (
    id                      UUID PRIMARY KEY,
    user_id                 UUID NOT NULL REFERENCES users (id),
    wallet_id               UUID NOT NULL REFERENCES wallets (id),
    provider                TEXT NOT NULL CHECK (provider IN ('mpesa', 'airtel')),
    provider_env            TEXT NOT NULL CHECK (provider_env IN ('simulated', 'sandbox', 'production')),
    msisdn                  TEXT NOT NULL,
    amount                  BIGINT NOT NULL CHECK (amount > 0),
    fee                     BIGINT NOT NULL DEFAULT 0 CHECK (fee >= 0),
    currency                CHAR(3) NOT NULL REFERENCES currencies (code),
    status                  TEXT NOT NULL CHECK (status IN ('created', 'pending', 'processing', 'succeeded', 'failed', 'reversed')),
    provider_status         TEXT NOT NULL DEFAULT 'not_sent'
                            CHECK (provider_status IN ('not_sent', 'sending', 'accepted', 'rejected', 'unknown',
                                                       'completed', 'failed')),
    outcome_uncertain       BOOLEAN NOT NULL DEFAULT false,
    provider_request_ref    TEXT,  -- our reference sent to provider (OriginatorConversationID / Airtel transaction.id)
    provider_conversation_ref TEXT,  -- provider-issued handle (Daraja ConversationID)
    provider_receipt        TEXT,  -- final provider transaction id
    provider_result_code    TEXT,
    provider_result_desc    TEXT,
    failure_reason          TEXT,
    hold_journal_id         UUID REFERENCES journal_transactions (id),
    settle_journal_id       UUID REFERENCES journal_transactions (id),
    release_journal_id      UUID REFERENCES journal_transactions (id),
    reversal_journal_id     UUID REFERENCES journal_transactions (id),
    needs_review            BOOLEAN NOT NULL DEFAULT false,
    dispatch_started_at     TIMESTAMPTZ,
    completed_at            TIMESTAMPTZ,
    last_polled_at          TIMESTAMPTZ,
    poll_count              INT NOT NULL DEFAULT 0,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT withdrawals_held CHECK (status = 'created' OR hold_journal_id IS NOT NULL),
    CONSTRAINT withdrawals_success_settled CHECK (status NOT IN ('succeeded', 'reversed') OR settle_journal_id IS NOT NULL),
    CONSTRAINT withdrawals_failed_released CHECK (status <> 'failed' OR release_journal_id IS NOT NULL),
    CONSTRAINT withdrawals_not_both CHECK (settle_journal_id IS NULL OR release_journal_id IS NULL)
);
CREATE UNIQUE INDEX withdrawals_request_ref_uidx ON withdrawals (provider, provider_request_ref) WHERE provider_request_ref IS NOT NULL;
CREATE UNIQUE INDEX withdrawals_conv_ref_uidx    ON withdrawals (provider, provider_conversation_ref) WHERE provider_conversation_ref IS NOT NULL;
CREATE UNIQUE INDEX withdrawals_receipt_uidx     ON withdrawals (provider, provider_receipt) WHERE provider_receipt IS NOT NULL;
CREATE INDEX withdrawals_user_idx ON withdrawals (user_id, created_at DESC);
CREATE INDEX withdrawals_open_idx ON withdrawals (status, updated_at) WHERE status IN ('pending', 'processing');

-- Every inbound provider callback is persisted verbatim before processing.
CREATE TABLE provider_callbacks (
    id           UUID PRIMARY KEY,
    provider     TEXT NOT NULL,
    kind         TEXT NOT NULL,
    dedupe_key   TEXT NOT NULL,
    payload      JSONB NOT NULL,
    source_ip    TEXT,
    status       TEXT NOT NULL DEFAULT 'received' CHECK (status IN ('received', 'processed', 'ignored', 'error')),
    result       TEXT,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ,
    UNIQUE (provider, kind, dedupe_key)
);
CREATE INDEX provider_callbacks_status_idx ON provider_callbacks (status, received_at);

-- Transactional outbox / durable job queue.
CREATE TABLE outbox_jobs (
    id           BIGSERIAL PRIMARY KEY,
    kind         TEXT NOT NULL,
    payload      JSONB NOT NULL,
    dedupe_key   TEXT UNIQUE,
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'done', 'dead')),
    attempts     INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 10,
    run_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until TIMESTAMPTZ,
    locked_by    TEXT,
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX outbox_jobs_ready_idx ON outbox_jobs (run_at) WHERE status IN ('pending', 'running');

-- +goose Down
DROP TABLE outbox_jobs;
DROP TABLE provider_callbacks;
DROP TABLE withdrawals;
DROP TABLE transfers;
DROP TABLE deposits;
DROP TABLE limit_rules;
DROP TABLE fee_rules;
DROP TABLE idempotency_keys;
