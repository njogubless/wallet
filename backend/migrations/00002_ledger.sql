-- +goose Up

-- Ledger accounts. `balance` is a projection maintained exclusively by the
-- ledger_entries insert trigger below; application code cannot write it.
-- For credit-normal accounts (liabilities such as customer wallets, revenue)
-- balance = credits - debits; for debit-normal accounts (assets such as
-- provider clearing/float) balance = debits - credits.
CREATE TABLE ledger_accounts (
    id             UUID PRIMARY KEY,
    code           TEXT UNIQUE,                      -- stable code for system accounts
    kind           TEXT NOT NULL CHECK (kind IN (
                       'wallet_available', 'wallet_hold',
                       'provider_clearing', 'fee_revenue', 'adjustments')),
    currency       CHAR(3) NOT NULL REFERENCES currencies (code),
    normal_side    TEXT NOT NULL CHECK (normal_side IN ('debit', 'credit')),
    allow_negative BOOLEAN NOT NULL DEFAULT false,
    balance        BIGINT NOT NULL DEFAULT 0,
    entry_count    BIGINT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, currency),
    CONSTRAINT ledger_accounts_non_negative CHECK (allow_negative OR balance >= 0),
    CONSTRAINT ledger_accounts_wallets_never_negative
        CHECK (kind NOT IN ('wallet_available', 'wallet_hold') OR allow_negative = false)
);

-- A journal transaction groups balanced entries. posting_key is a unique,
-- deterministic key (e.g. "deposit:<id>:settle") so that retries can never
-- post the same business event twice.
CREATE TABLE journal_transactions (
    id                  UUID PRIMARY KEY,
    posting_key         TEXT NOT NULL UNIQUE,
    kind                TEXT NOT NULL CHECK (kind IN (
                            'deposit', 'deposit_reversal',
                            'transfer', 'transfer_reversal',
                            'withdrawal_hold', 'withdrawal_settle', 'withdrawal_release', 'withdrawal_reversal',
                            'adjustment')),
    reference_type      TEXT NOT NULL,
    reference_id        UUID NOT NULL,
    reverses_journal_id UUID REFERENCES journal_transactions (id),
    description         TEXT NOT NULL DEFAULT '',
    created_by          UUID,
    posted_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX journal_transactions_reference_idx ON journal_transactions (reference_type, reference_id);
CREATE UNIQUE INDEX journal_transactions_single_reversal_idx
    ON journal_transactions (reverses_journal_id) WHERE reverses_journal_id IS NOT NULL;

CREATE TABLE ledger_entries (
    id         BIGSERIAL PRIMARY KEY,
    journal_id UUID NOT NULL REFERENCES journal_transactions (id),
    account_id UUID NOT NULL,
    currency   CHAR(3) NOT NULL,
    direction  TEXT NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount     BIGINT NOT NULL CHECK (amount > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Composite FK guarantees an entry's currency equals its account's currency.
    FOREIGN KEY (account_id, currency) REFERENCES ledger_accounts (id, currency)
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, id DESC);
CREATE INDEX ledger_entries_journal_idx ON ledger_entries (journal_id);

-- Maintain the balance projection atomically with each entry insert.
-- +goose StatementBegin
CREATE FUNCTION ledger_apply_entry() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_normal TEXT;
BEGIN
    SELECT normal_side INTO v_normal FROM ledger_accounts WHERE id = NEW.account_id;
    PERFORM set_config('wallet.ledger_posting', 'on', true);
    UPDATE ledger_accounts
       SET balance = balance + CASE WHEN NEW.direction = v_normal THEN NEW.amount ELSE -NEW.amount END,
           entry_count = entry_count + 1
     WHERE id = NEW.account_id;
    PERFORM set_config('wallet.ledger_posting', 'off', true);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER ledger_entries_apply AFTER INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_apply_entry();

-- Reject any direct write to a projection column outside ledger posting.
-- +goose StatementBegin
CREATE FUNCTION ledger_accounts_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.balance IS DISTINCT FROM OLD.balance OR NEW.entry_count IS DISTINCT FROM OLD.entry_count)
       AND coalesce(current_setting('wallet.ledger_posting', true), 'off') <> 'on' THEN
        RAISE EXCEPTION 'ledger account balances can only change through ledger entries'
            USING ERRCODE = 'P0001';
    END IF;
    IF NEW.currency IS DISTINCT FROM OLD.currency OR NEW.normal_side IS DISTINCT FROM OLD.normal_side
       OR NEW.kind IS DISTINCT FROM OLD.kind THEN
        RAISE EXCEPTION 'ledger account identity is immutable' USING ERRCODE = 'P0001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER ledger_accounts_guard BEFORE UPDATE ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION ledger_accounts_guard();

-- New accounts always start at zero.
-- +goose StatementBegin
CREATE FUNCTION ledger_accounts_zero_on_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.balance := 0;
    NEW.entry_count := 0;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER ledger_accounts_zero_on_insert BEFORE INSERT ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION ledger_accounts_zero_on_insert();

-- Every journal must balance per currency; checked at COMMIT so that all
-- entries of the journal are visible.
-- +goose StatementBegin
CREATE FUNCTION ledger_check_journal_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_bad RECORD;
BEGIN
    SELECT currency,
           sum(CASE WHEN direction = 'debit' THEN amount ELSE 0 END) AS debits,
           sum(CASE WHEN direction = 'credit' THEN amount ELSE 0 END) AS credits
      INTO v_bad
      FROM ledger_entries
     WHERE journal_id = NEW.journal_id
     GROUP BY currency
    HAVING sum(CASE WHEN direction = 'debit' THEN amount ELSE -amount END) <> 0
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'journal % is unbalanced in %: debits % credits %',
            NEW.journal_id, v_bad.currency, v_bad.debits, v_bad.credits
            USING ERRCODE = 'P0001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER ledger_entries_balanced AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_journal_balanced();

-- A journal header without entries is meaningless; require at least two entries.
-- +goose StatementBegin
CREATE FUNCTION ledger_check_journal_has_entries() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT count(*) FROM ledger_entries WHERE journal_id = NEW.id) < 2 THEN
        RAISE EXCEPTION 'journal % must have at least two entries', NEW.id USING ERRCODE = 'P0001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER journal_transactions_has_entries AFTER INSERT ON journal_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_journal_has_entries();

-- Posted records are append-only.
-- +goose StatementBegin
CREATE FUNCTION reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'P0001';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER ledger_entries_immutable BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER ledger_entries_no_truncate BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER journal_transactions_immutable BEFORE UPDATE OR DELETE ON journal_transactions
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER journal_transactions_no_truncate BEFORE TRUNCATE ON journal_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Drift report: projection vs. sum of entries. Must always be empty.
CREATE VIEW ledger_projection_drift AS
SELECT a.id AS account_id, a.code, a.kind, a.currency, a.balance AS projected_balance,
       coalesce(sum(CASE WHEN e.direction = a.normal_side THEN e.amount ELSE -e.amount END), 0) AS derived_balance
  FROM ledger_accounts a
  LEFT JOIN ledger_entries e ON e.account_id = a.id
 GROUP BY a.id
HAVING a.balance <> coalesce(sum(CASE WHEN e.direction = a.normal_side THEN e.amount ELSE -e.amount END), 0);

-- Customer wallets. Each wallet owns two ledger accounts: spendable funds and
-- funds held for in-flight withdrawals.
CREATE TABLE wallets (
    id                   UUID PRIMARY KEY,
    user_id              UUID NOT NULL REFERENCES users (id),
    account_number       TEXT NOT NULL UNIQUE,
    currency             CHAR(3) NOT NULL REFERENCES currencies (code),
    status               TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen', 'closed')),
    available_account_id UUID NOT NULL UNIQUE,
    hold_account_id      UUID NOT NULL UNIQUE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, currency),
    FOREIGN KEY (available_account_id, currency) REFERENCES ledger_accounts (id, currency),
    FOREIGN KEY (hold_account_id, currency) REFERENCES ledger_accounts (id, currency)
);

-- System accounts (KES). Provider clearing accounts are assets representing
-- funds held at the provider; they may go negative only in the books when
-- payouts are funded from a separate float, which reconciliation will surface.
INSERT INTO ledger_accounts (id, code, kind, currency, normal_side, allow_negative) VALUES
    ('00000000-0000-0000-0000-000000000101', 'mpesa_clearing_kes',  'provider_clearing', 'KES', 'debit',  true),
    ('00000000-0000-0000-0000-000000000102', 'airtel_clearing_kes', 'provider_clearing', 'KES', 'debit',  true),
    ('00000000-0000-0000-0000-000000000201', 'fee_revenue_kes',     'fee_revenue',       'KES', 'credit', true),
    ('00000000-0000-0000-0000-000000000301', 'adjustments_kes',     'adjustments',       'KES', 'debit',  true);

-- +goose Down
DROP TABLE wallets;
DROP VIEW ledger_projection_drift;
DROP TABLE ledger_entries;
DROP TABLE journal_transactions;
DROP TABLE ledger_accounts;
DROP FUNCTION reject_mutation();
DROP FUNCTION ledger_check_journal_has_entries();
DROP FUNCTION ledger_check_journal_balanced();
DROP FUNCTION ledger_accounts_zero_on_insert();
DROP FUNCTION ledger_accounts_guard();
DROP FUNCTION ledger_apply_entry();
