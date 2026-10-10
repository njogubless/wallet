-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;

-- Supported currencies and their precision. Amounts everywhere are stored as
-- BIGINT minor units; minor_units says how many decimal places one major unit has.
CREATE TABLE currencies (
    code        CHAR(3) PRIMARY KEY CHECK (code ~ '^[A-Z]{3}$'),
    minor_units SMALLINT NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    name        TEXT NOT NULL
);
INSERT INTO currencies (code, minor_units, name) VALUES ('KES', 2, 'Kenyan Shilling');

CREATE TABLE users (
    id                    UUID PRIMARY KEY,
    phone                 TEXT NOT NULL UNIQUE CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
    email                 CITEXT UNIQUE,
    full_name             TEXT NOT NULL CHECK (char_length(full_name) BETWEEN 1 AND 120),
    password_hash         TEXT NOT NULL,
    pin_hash              TEXT,
    pin_failed_attempts   INT NOT NULL DEFAULT 0,
    pin_locked_until      TIMESTAMPTZ,
    failed_login_attempts INT NOT NULL DEFAULT 0,
    login_locked_until    TIMESTAMPTZ,
    role                  TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'support', 'admin')),
    status                TEXT NOT NULL DEFAULT 'pending_verification'
                          CHECK (status IN ('pending_verification', 'active', 'restricted', 'suspended', 'closed')),
    status_reason         TEXT,
    kyc_tier              SMALLINT NOT NULL DEFAULT 1 CHECK (kyc_tier BETWEEN 0 AND 3),
    phone_verified_at     TIMESTAMPTZ,
    email_verified_at     TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX users_created_at_idx ON users (created_at DESC);

-- Opaque refresh tokens; only a SHA-256 hash is stored. Tokens rotate on every
-- use, and presenting an already-rotated token revokes the whole family.
CREATE TABLE refresh_tokens (
    id          UUID PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users (id),
    family_id   UUID NOT NULL,
    token_hash  BYTEA NOT NULL UNIQUE,
    expires_at  TIMESTAMPTZ NOT NULL,
    rotated_at  TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ,
    user_agent  TEXT,
    ip          TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);

-- One-time codes for phone verification and password reset (hash only).
CREATE TABLE verification_codes (
    id           UUID PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users (id),
    purpose      TEXT NOT NULL CHECK (purpose IN ('verify_phone', 'reset_password')),
    code_hash    BYTEA NOT NULL,
    attempts     INT NOT NULL DEFAULT 0,
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX verification_codes_lookup_idx ON verification_codes (user_id, purpose, created_at DESC);

-- +goose Down
DROP TABLE verification_codes;
DROP TABLE refresh_tokens;
DROP TABLE users;
DROP TABLE currencies;
