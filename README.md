# Sovereign Trust Wallet

A mobile wallet platform for Kenya: Go backend with a PostgreSQL double-entry
ledger, M-Pesa Daraja and Airtel Money adapters, and a Flutter app (customer
app + operations console). UI follows the Stitch "Sovereign Trust" design in
`stitch_east_african_digital_wallet_platform/`.

> **Status:** functional end-to-end against a **deterministic payment
> simulator**. Provider adapters are implemented but **have not been run
> against Safaricom or Airtel sandboxes** and their contracts were not
> re-verified against official documentation (see `docs/providers/`). This
> is not a licensed or compliance-reviewed service.

## Repository layout

```
backend/            Go API, worker, operator CLI, migrations, OpenAPI
  api/openapi.yaml  API contract
  cmd/{api,worker,walletctl}
  internal/...      see docs/ARCHITECTURE.md
  migrations/       goose SQL migrations (embedded)
mobile/             Flutter app: lib/main.dart (customer), lib/main_admin.dart (console)
docs/               architecture, security, providers, reconciliation, runbooks, plan
docker-compose.yml  postgres, redis, migrate, api, worker, swagger (profile: docs)
```

## Quick start (simulated payments)

Requirements: Docker, Go 1.24+, Flutter 3.41+.

```bash
cp .env.example .env                       # placeholders only; set JWT_SECRET
make up                                    # postgres, redis, migrations, api (:8080), worker
curl localhost:8080/readyz
```

If ports 5432/6379 are taken: `WALLET_PG_PORT=55441 WALLET_REDIS_PORT=56391 make up`.

`PAYMENTS_MODE=simulated` never contacts a provider. Simulated records are
stored with `provider_env = 'simulated'`, receipts start with `SIM`, and the
app shows a "Simulated payments" banner. Outcomes are chosen by the last four
digits of the phone number: `0000` rejected, `1111` customer cancels / payout
fails, `2222` timeout (uncertain), `3333` never resolves, anything else
succeeds after ~3 s. Simulated mode is refused when `APP_ENV=production`.

Phone verification codes are printed in the API log in development
(`SMS_MODE=log`): `docker compose logs api | grep "verification code"`.

Grant a staff role to a registered user:

```bash
make grant-admin PHONE=0712345678 ROLE=admin   # or ROLE=support
```

## Running things

| Task | Command |
|---|---|
| Start / stop stack | `make up` / `make down` |
| Apply migrations | `make migrate` (uses `DATABASE_URL`) or `docker compose run --rm migrate` |
| Run API / worker locally | `make api`, `make worker` (exports `.env`) |
| Backend unit tests | `make test-unit` (integration tests skip without a DB) |
| Backend full suite (race detector, real PostgreSQL) | `make test` (`TEST_DATABASE_URL` must point to a DB the tests may create databases from) |
| Static analysis | `make lint`, `make flutter-analyze` |
| Flutter tests | `make flutter-test` |
| API docs (Swagger UI on :8081) | `make docs` |
| Customer app | `cd mobile && flutter run --dart-define=API_BASE_URL=http://10.0.2.2:8080` (Android emulator) |
| Ops console (web) | `cd mobile && flutter run -d chrome -t lib/main_admin.dart --dart-define=API_BASE_URL=http://localhost:8080` and set `CORS_ALLOWED_ORIGINS` to the console origin |
| Metrics | API `:9090/metrics`, worker `:9091/metrics` |

## Key properties

* Balances are a trigger-maintained projection of an append-only double-entry
  ledger; the database rejects overdrafts, unbalanced journals, and direct
  balance writes.
* Every money-moving request is idempotent (key completed in the same
  transaction as the money movement).
* Provider calls go through a transactional outbox; uncertain outcomes are
  never retried blindly, and held funds are released only on definitive
  failure.
* Callbacks are hints; outcomes come from authenticated provider status
  queries (or strictly matched B2C results).
* Staff can never edit balances: adjustments and reversals are reason-coded,
  maker-checker, audited ledger journals.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md),
[docs/SECURITY.md](docs/SECURITY.md),
[docs/IMPLEMENTATION_PLAN.md](docs/IMPLEMENTATION_PLAN.md) and
[docs/runbooks/pending-payment.md](docs/runbooks/pending-payment.md).
