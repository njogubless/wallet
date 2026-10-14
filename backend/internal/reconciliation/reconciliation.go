// Package reconciliation compares internal records and ledger postings with
// each other and with provider statements, and records discrepancies for
// investigation. It never edits balances or guesses whether money moved:
// corrections go through status re-checks, admin resolution of uncertain
// payouts, or the maker-checker financial request workflow.
package reconciliation

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/audit"
	"github.com/sovereigntrust/wallet/internal/db"
	"github.com/sovereigntrust/wallet/internal/ledger"
	"github.com/sovereigntrust/wallet/internal/money"
)

var (
	openItems = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "wallet_reconciliation_open_items", Help: "Open reconciliation discrepancies by type.",
	}, []string{"discrepancy"})
	oldestPending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "wallet_pending_oldest_age_seconds", Help: "Age of the oldest non-terminal transaction by kind.",
	}, []string{"kind"})
	pendingCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "wallet_pending_transactions", Help: "Non-terminal transactions by kind and status.",
	}, []string{"kind", "status"})
)

func init() { prometheus.MustRegister(openItems, oldestPending, pendingCount) }

type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

type Run struct {
	ID         uuid.UUID       `json:"id"`
	Source     string          `json:"source"`
	Status     string          `json:"status"`
	Stats      json.RawMessage `json:"stats"`
	StartedBy  *uuid.UUID      `json:"started_by"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	Error      *string         `json:"error"`
}

type Item struct {
	ID             uuid.UUID       `json:"id"`
	RunID          uuid.UUID       `json:"run_id"`
	Discrepancy    string          `json:"discrepancy"`
	EntityType     *string         `json:"entity_type"`
	EntityID       *string         `json:"entity_id"`
	Provider       *string         `json:"provider"`
	ProviderRef    *string         `json:"provider_ref"`
	Details        json.RawMessage `json:"details"`
	Status         string          `json:"status"`
	ResolutionNote *string         `json:"resolution_note"`
	ResolvedBy     *uuid.UUID      `json:"resolved_by"`
	ResolvedAt     *time.Time      `json:"resolved_at"`
	CreatedAt      time.Time       `json:"created_at"`
}

type finding struct {
	discrepancy, entityType, entityID, provider, providerRef string
	details                                                  map[string]any
}

func (f finding) fingerprint() string {
	return strings.Join([]string{f.discrepancy, f.entityType, f.entityID, f.provider, f.providerRef}, "|")
}

func (s *Service) startRun(ctx context.Context, source string, by *uuid.UUID) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	_, err := s.Pool.Exec(ctx, `INSERT INTO reconciliation_runs (id, source, status, started_by) VALUES ($1, $2, 'running', $3)`, id, source, by)
	return id, err
}

func (s *Service) finishRun(ctx context.Context, runID uuid.UUID, findings []finding, extra map[string]any, runErr error) (*Run, error) {
	created := 0
	if runErr == nil {
		err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
			for _, f := range findings {
				d, _ := json.Marshal(f.details)
				tag, err := tx.Exec(ctx, `
					INSERT INTO reconciliation_items (id, run_id, discrepancy, entity_type, entity_id, provider, provider_ref, details, fingerprint)
					VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), NULLIF($6,''), NULLIF($7,''), $8, $9)
					ON CONFLICT (fingerprint) WHERE status IN ('open', 'investigating') DO NOTHING`,
					uuid.Must(uuid.NewV7()), runID, f.discrepancy, f.entityType, f.entityID, f.provider, f.providerRef, d, f.fingerprint())
				if err != nil {
					return err
				}
				created += int(tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			runErr = err
		}
	}
	stats := map[string]any{"findings": len(findings), "new_items": created}
	for k, v := range extra {
		stats[k] = v
	}
	statsJSON, _ := json.Marshal(stats)
	status := "completed"
	var errText *string
	if runErr != nil {
		status = "failed"
		e := runErr.Error()
		errText = &e
	}
	_, err := s.Pool.Exec(ctx, `UPDATE reconciliation_runs SET status = $2, stats = $3, error = $4, finished_at = now() WHERE id = $1`,
		runID, status, statsJSON, errText)
	if err != nil {
		return nil, err
	}
	if runErr != nil {
		return nil, runErr
	}
	return s.GetRun(ctx, runID)
}

// RunInternal checks ledger integrity and internal transaction states.
func (s *Service) RunInternal(ctx context.Context, by *uuid.UUID) (*Run, error) {
	runID, err := s.startRun(ctx, "internal", by)
	if err != nil {
		return nil, err
	}
	findings, err := s.internalFindings(ctx)
	return s.finishRun(ctx, runID, findings, nil, err)
}

func (s *Service) internalFindings(ctx context.Context) ([]finding, error) {
	var out []finding
	drift, err := ledger.Drift(ctx, s.Pool)
	if err != nil {
		return nil, err
	}
	for _, d := range drift {
		out = append(out, finding{discrepancy: "projection_drift", entityType: "ledger_account", entityID: d.AccountID.String(),
			details: map[string]any{"projected": d.Projected, "derived": d.Derived, "kind": d.Kind, "currency": d.Currency}})
	}
	tb, err := ledger.TrialBalance(ctx, s.Pool)
	if err != nil {
		return nil, err
	}
	for cur, v := range tb {
		if v != 0 {
			out = append(out, finding{discrepancy: "projection_drift", entityType: "trial_balance", entityID: cur,
				details: map[string]any{"net": v}})
		}
	}
	queries := []struct {
		discrepancy, entityType, sql string
	}{
		// Deposit postings must correspond to a succeeded/reversed deposit.
		{"posting_without_evidence", "journal", `
			SELECT j.id::text, coalesce(d.provider, ''), coalesce(d.provider_receipt, ''), jsonb_build_object('reference_id', j.reference_id, 'deposit_status', d.status)
			  FROM journal_transactions j LEFT JOIN deposits d ON d.id = j.reference_id
			 WHERE j.kind = 'deposit' AND j.reference_type = 'deposit' AND (d.id IS NULL OR d.status NOT IN ('succeeded', 'reversed'))`},
		// Succeeded deposits without a provider receipt after a day.
		{"posting_without_evidence", "deposit", `
			SELECT id::text, provider, '', jsonb_build_object('amount', amount, 'reason', 'no provider receipt recorded')
			  FROM deposits WHERE status = 'succeeded' AND provider_receipt IS NULL AND completed_at < now() - interval '1 day'`},
		{"posting_without_evidence", "withdrawal", `
			SELECT id::text, provider, '', jsonb_build_object('amount', amount, 'reason', 'no provider receipt recorded')
			  FROM withdrawals WHERE status = 'succeeded' AND provider_receipt IS NULL AND completed_at < now() - interval '1 day'`},
		// Deposits that expired without a definitive provider answer.
		{"stale_pending", "deposit", `
			SELECT id::text, provider, coalesce(provider_checkout_ref, provider_request_ref, ''), jsonb_build_object('status', status, 'provider_status', provider_status, 'amount', amount)
			  FROM deposits WHERE (status = 'expired' AND needs_review) OR (status = 'pending_provider' AND expires_at < now() - interval '30 minutes')`},
		// Succeeded but flagged (duplicate receipt, callback mismatch, late success).
		{"status_mismatch", "deposit", `
			SELECT id::text, provider, coalesce(provider_receipt, ''), jsonb_build_object('status', status, 'reason', 'flagged for review during processing')
			  FROM deposits WHERE status = 'succeeded' AND needs_review`},
		{"uncertain_withdrawal", "withdrawal", `
			SELECT id::text, provider, coalesce(provider_conversation_ref, provider_request_ref, ''), jsonb_build_object('status', status, 'provider_status', provider_status, 'amount', amount, 'uncertain', outcome_uncertain)
			  FROM withdrawals WHERE status = 'processing' AND (outcome_uncertain OR needs_review)`},
		{"status_mismatch", "withdrawal", `
			SELECT id::text, provider, coalesce(provider_receipt, ''), jsonb_build_object('status', status, 'reason', 'conflicting provider outcomes')
			  FROM withdrawals WHERE status IN ('succeeded', 'failed') AND needs_review`},
		{"stale_pending", "withdrawal", `
			SELECT id::text, provider, '', jsonb_build_object('status', status, 'amount', amount)
			  FROM withdrawals WHERE status = 'pending' AND created_at < now() - interval '1 hour'`},
		// The same provider receipt used by a deposit and a withdrawal.
		{"duplicate_provider_ref", "provider_receipt", `
			SELECT d.provider_receipt, d.provider, d.provider_receipt, jsonb_build_object('deposit_id', d.id, 'withdrawal_id', w.id)
			  FROM deposits d JOIN withdrawals w ON w.provider = d.provider AND w.provider_receipt = d.provider_receipt`},
	}
	for _, q := range queries {
		rows, err := s.Pool.Query(ctx, q.sql)
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", q.discrepancy, q.entityType, err)
		}
		for rows.Next() {
			var id, provider, ref string
			var details map[string]any
			if err := rows.Scan(&id, &provider, &ref, &details); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, finding{discrepancy: q.discrepancy, entityType: q.entityType, entityID: id, provider: provider, providerRef: ref, details: details})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// StatementRow is a normalized provider statement line. Provider-specific
// exports (e.g. M-Pesa org portal CSV) should be converted to this format;
// see docs/reconciliation.md.
type StatementRow struct {
	Provider   string
	Receipt    string
	Reference  string // our provider_request_ref if the provider echoes it
	Direction  string // "in" (collection) or "out" (payout)
	Amount     int64
	Currency   string
	Status     string // "completed" or "failed"
	OccurredAt time.Time
}

var statementHeader = []string{"provider", "receipt", "reference", "direction", "amount", "currency", "status", "occurred_at"}

// ParseStatementCSV parses the normalized statement CSV format.
func ParseStatementCSV(r io.Reader, maxRows int) ([]StatementRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = len(statementHeader)
	head, err := cr.Read()
	if err != nil {
		return nil, apperr.Validation("statement: cannot read header", nil)
	}
	for i, h := range statementHeader {
		if strings.TrimSpace(strings.ToLower(head[i])) != h {
			return nil, apperr.Validation("statement header must be: "+strings.Join(statementHeader, ","), nil)
		}
	}
	var out []StatementRow
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, apperr.Validation(fmt.Sprintf("statement line %d: %v", line, err), nil)
		}
		if len(out) >= maxRows {
			return nil, apperr.Validation(fmt.Sprintf("statement exceeds %d rows", maxRows), nil)
		}
		bad := func(msg string) error { return apperr.Validation(fmt.Sprintf("statement line %d: %s", line, msg), nil) }
		row := StatementRow{Provider: strings.TrimSpace(rec[0]), Receipt: strings.TrimSpace(rec[1]), Reference: strings.TrimSpace(rec[2]),
			Direction: strings.TrimSpace(rec[3]), Currency: strings.ToUpper(strings.TrimSpace(rec[5])), Status: strings.TrimSpace(rec[6])}
		if row.Provider != "mpesa" && row.Provider != "airtel" {
			return nil, bad("provider must be mpesa or airtel")
		}
		if row.Receipt == "" {
			return nil, bad("receipt required")
		}
		if row.Direction != "in" && row.Direction != "out" {
			return nil, bad("direction must be in or out")
		}
		if row.Status != "completed" && row.Status != "failed" {
			return nil, bad("status must be completed or failed")
		}
		cur, err := money.LookupCurrency(row.Currency)
		if err != nil {
			// Keep the row; currency mismatches are a finding, not a parse error.
			cur = money.Currency{Code: row.Currency, MinorUnits: 2}
		}
		row.Amount, err = money.ParsePositive(strings.TrimSpace(rec[4]), cur)
		if err != nil {
			return nil, bad("amount: " + err.Error())
		}
		row.OccurredAt, err = time.Parse(time.RFC3339, strings.TrimSpace(rec[7]))
		if err != nil {
			return nil, bad("occurred_at must be RFC3339")
		}
		out = append(out, row)
	}
	return out, nil
}

type internalTxn struct {
	entityType, id, status, currency, receipt string
	amount                                    int64
}

func (s *Service) findInternal(ctx context.Context, r StatementRow) (*internalTxn, error) {
	table, entity := "deposits", "deposit"
	if r.Direction == "out" {
		table, entity = "withdrawals", "withdrawal"
	}
	var t internalTxn
	var receipt *string
	err := s.Pool.QueryRow(ctx, `
		SELECT id::text, status, currency, amount, provider_receipt FROM `+table+`
		 WHERE provider = $1 AND (provider_receipt = $2 OR ($3 <> '' AND provider_request_ref = $3))
		 ORDER BY (provider_receipt = $2) DESC NULLS LAST LIMIT 1`, r.Provider, r.Receipt, r.Reference).
		Scan(&t.id, &t.status, &t.currency, &t.amount, &receipt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.entityType = entity
	if receipt != nil {
		t.receipt = *receipt
	}
	return &t, nil
}

// ImportStatement reconciles provider statement rows against internal records.
func (s *Service) ImportStatement(ctx context.Context, provider string, rows []StatementRow, by *uuid.UUID) (*Run, error) {
	runID, err := s.startRun(ctx, provider+"_statement", by)
	if err != nil {
		return nil, err
	}
	findings, extra, err := s.statementFindings(ctx, provider, rows)
	if err == nil && by != nil {
		_ = audit.Record(ctx, s.Pool, audit.Event{Action: "reconciliation.statement_imported", TargetType: "reconciliation_run",
			TargetID: runID.String(), Details: map[string]any{"provider": provider, "rows": len(rows)}})
	}
	return s.finishRun(ctx, runID, findings, extra, err)
}

func (s *Service) statementFindings(ctx context.Context, provider string, rows []StatementRow) ([]finding, map[string]any, error) {
	var out []finding
	seen := map[string]int{}
	var minT, maxT time.Time
	matched := 0
	for _, r := range rows {
		if r.Provider != provider {
			return nil, nil, apperr.Validation("all statement rows must be for provider "+provider, nil)
		}
		seen[r.Receipt]++
		if seen[r.Receipt] == 2 {
			out = append(out, finding{discrepancy: "duplicate_provider_ref", entityType: "statement", provider: provider, providerRef: r.Receipt,
				details: map[string]any{"reason": "receipt appears more than once in statement"}})
		}
		if minT.IsZero() || r.OccurredAt.Before(minT) {
			minT = r.OccurredAt
		}
		if r.OccurredAt.After(maxT) {
			maxT = r.OccurredAt
		}
		t, err := s.findInternal(ctx, r)
		if err != nil {
			return nil, nil, err
		}
		base := map[string]any{"statement_amount": r.Amount, "statement_currency": r.Currency, "statement_status": r.Status,
			"direction": r.Direction, "occurred_at": r.OccurredAt}
		if t == nil {
			if r.Status == "completed" {
				out = append(out, finding{discrepancy: "missing_internal", entityType: "statement", provider: provider, providerRef: r.Receipt, details: base})
			}
			continue
		}
		matched++
		base["internal_status"], base["internal_amount"], base["internal_currency"] = t.status, t.amount, t.currency
		f := finding{entityType: t.entityType, entityID: t.id, provider: provider, providerRef: r.Receipt, details: base}
		switch {
		case t.currency != r.Currency:
			f.discrepancy = "currency_mismatch"
		case t.amount != r.Amount:
			f.discrepancy = "amount_mismatch"
		case r.Status == "completed" && (t.status == "pending_provider" || t.status == "expired" || t.status == "created" ||
			t.status == "pending" || t.status == "processing"):
			f.discrepancy = "pending_but_completed"
		case r.Status == "completed" && t.status == "failed":
			f.discrepancy = "status_mismatch"
		case r.Status == "failed" && (t.status == "succeeded"):
			f.discrepancy = "status_mismatch"
		case r.Status == "completed" && t.status == "succeeded" && t.receipt == "":
			// Matched by reference; record the receipt gap for follow-up.
			f.discrepancy = "posting_without_evidence"
			base["reason"] = "internal record lacks provider receipt"
		default:
			continue
		}
		out = append(out, f)
	}
	// Internal successes inside the statement window that the statement lacks.
	if !minT.IsZero() {
		present := make([]string, 0, len(seen))
		for k := range seen {
			present = append(present, k)
		}
		for _, q := range []struct{ table, entity string }{{"deposits", "deposit"}, {"withdrawals", "withdrawal"}} {
			rows, err := s.Pool.Query(ctx, `
				SELECT id::text, coalesce(provider_receipt, ''), amount FROM `+q.table+`
				 WHERE provider = $1 AND status = 'succeeded' AND completed_at BETWEEN $2 AND $3
				   AND (provider_receipt IS NULL OR NOT (provider_receipt = ANY($4)))`, provider, minT, maxT, present)
			if err != nil {
				return nil, nil, err
			}
			for rows.Next() {
				var id, rc string
				var amt int64
				if err := rows.Scan(&id, &rc, &amt); err != nil {
					rows.Close()
					return nil, nil, err
				}
				out = append(out, finding{discrepancy: "posting_without_evidence", entityType: q.entity, entityID: id, provider: provider, providerRef: rc,
					details: map[string]any{"amount": amt, "reason": "succeeded internally but absent from provider statement"}})
			}
			rows.Close()
		}
	}
	return out, map[string]any{"rows": len(rows), "matched": matched}, nil
}

const runCols = `id, source, status, stats, started_by, started_at, finished_at, error`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Source, &r.Status, &r.Stats, &r.StartedBy, &r.StartedAt, &r.FinishedAt, &r.Error)
	return &r, err
}

func (s *Service) GetRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	r, err := scanRun(s.Pool.QueryRow(ctx, `SELECT `+runCols+` FROM reconciliation_runs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("reconciliation run")
	}
	return r, err
}

func (s *Service) ListRuns(ctx context.Context, limit int) ([]*Run, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+runCols+` FROM reconciliation_runs ORDER BY started_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Run, error) { return scanRun(r) })
}

const itemCols = `id, run_id, discrepancy, entity_type, entity_id, provider, provider_ref, details, status, resolution_note, resolved_by, resolved_at, created_at`

func scanItem(row pgx.Row) (*Item, error) {
	var i Item
	err := row.Scan(&i.ID, &i.RunID, &i.Discrepancy, &i.EntityType, &i.EntityID, &i.Provider, &i.ProviderRef, &i.Details,
		&i.Status, &i.ResolutionNote, &i.ResolvedBy, &i.ResolvedAt, &i.CreatedAt)
	return &i, err
}

func (s *Service) ListItems(ctx context.Context, status, discrepancy string, limit int) ([]*Item, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+itemCols+` FROM reconciliation_items
		 WHERE ($1 = '' OR status = $1) AND ($2 = '' OR discrepancy = $2)
		 ORDER BY created_at DESC LIMIT $3`, status, discrepancy, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Item, error) { return scanItem(r) })
}

// UpdateItem moves an item through open → investigating → resolved|dismissed.
// Resolution is a record of the investigation; it does not move money.
func (s *Service) UpdateItem(ctx context.Context, id uuid.UUID, status, note string, actor uuid.UUID) (*Item, error) {
	if status != "investigating" && status != "resolved" && status != "dismissed" {
		return nil, apperr.Field("status", "must be investigating, resolved or dismissed")
	}
	note = strings.TrimSpace(note)
	if status != "investigating" && len(note) < 10 {
		return nil, apperr.Field("note", "explain the resolution (at least 10 characters)")
	}
	var item *Item
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var cur string
		if err := tx.QueryRow(ctx, `SELECT status FROM reconciliation_items WHERE id = $1 FOR UPDATE`, id).Scan(&cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.NotFound("reconciliation item")
			}
			return err
		}
		if cur == "resolved" || cur == "dismissed" {
			return apperr.New(apperr.CodeInvalidState, "item already closed")
		}
		var err error
		item, err = scanItem(tx.QueryRow(ctx, `
			UPDATE reconciliation_items SET status = $2, resolution_note = NULLIF($3, ''),
			       resolved_by = CASE WHEN $2 IN ('resolved','dismissed') THEN $4::uuid END,
			       resolved_at = CASE WHEN $2 IN ('resolved','dismissed') THEN now() END
			 WHERE id = $1 RETURNING `+itemCols, id, status, note, actor))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "reconciliation.item_" + status, TargetType: "reconciliation_item",
			TargetID: id.String(), Details: map[string]any{"note": note}})
	})
	return item, err
}

// CollectGauges refreshes operational gauges (pending ages, open items).
func (s *Service) CollectGauges(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `SELECT discrepancy, count(*) FROM reconciliation_items WHERE status IN ('open','investigating') GROUP BY 1`)
	if err != nil {
		return err
	}
	openItems.Reset()
	for rows.Next() {
		var d string
		var n float64
		if err := rows.Scan(&d, &n); err != nil {
			rows.Close()
			return err
		}
		openItems.WithLabelValues(d).Set(n)
	}
	rows.Close()
	pendingCount.Reset()
	for _, q := range []struct{ kind, sql string }{
		{"deposit", `SELECT status, count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0) FROM deposits WHERE status IN ('created','pending_provider') GROUP BY status`},
		{"withdrawal", `SELECT status, count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0) FROM withdrawals WHERE status IN ('pending','processing') GROUP BY status`},
	} {
		rows, err := s.Pool.Query(ctx, q.sql)
		if err != nil {
			return err
		}
		oldest := 0.0
		for rows.Next() {
			var st string
			var n, age float64
			if err := rows.Scan(&st, &n, &age); err != nil {
				rows.Close()
				return err
			}
			pendingCount.WithLabelValues(q.kind, st).Set(n)
			oldest = max(oldest, age)
		}
		rows.Close()
		oldestPending.WithLabelValues(q.kind).Set(oldest)
	}
	return nil
}
