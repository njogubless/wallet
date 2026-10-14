// Package statements generates downloadable wallet statements (CSV and PDF).
// Opening and closing balances are derived from ledger entries, not from the
// activity feed, so the statement reconciles to the books.
package statements

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/audit"
	"github.com/sovereigntrust/wallet/internal/money"
	"github.com/sovereigntrust/wallet/internal/wallet"
)

var nairobi = func() *time.Location {
	if l, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return l
	}
	return time.FixedZone("EAT", 3*3600)
}()

type Statement struct {
	ID          uuid.UUID `json:"id"`
	WalletID    uuid.UUID `json:"wallet_id"`
	PeriodStart string    `json:"period_start"`
	PeriodEnd   string    `json:"period_end"`
	Format      string    `json:"format"`
	SHA256      string    `json:"sha256"`
	SizeBytes   int       `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
}

type Service struct{ Pool *pgxpool.Pool }

type line struct {
	At          time.Time
	Ref         string
	Type        string
	Description string
	Debit       int64
	Credit      int64
	Fee         int64
	Status      string
}

func describe(a wallet.Activity) string {
	switch a.Type {
	case "deposit":
		return "Deposit via " + deref(a.Provider)
	case "withdrawal":
		return "Withdrawal to " + deref(a.Provider)
	case "transfer_out":
		return "Transfer to " + deref(a.Counterparty)
	case "transfer_in":
		return "Transfer from " + deref(a.Counterparty)
	case "adjustment":
		return "Adjustment: " + a.Description
	}
	return a.Type
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Generate builds a statement for [from, to] (calendar dates, EAT).
func (s *Service) Generate(ctx context.Context, userID uuid.UUID, from, to time.Time, format string) (*Statement, error) {
	if format != "csv" && format != "pdf" {
		return nil, apperr.Field("format", "must be csv or pdf")
	}
	if to.Before(from) {
		return nil, apperr.Field("to", "must not be before from")
	}
	if to.Sub(from) > 366*24*time.Hour {
		return nil, apperr.Field("to", "statement period may not exceed one year")
	}
	w, err := wallet.PrimaryForUser(ctx, s.Pool, userID, money.KES.Code)
	if err != nil {
		return nil, err
	}
	cur, _ := money.LookupCurrency(w.Currency)
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, nairobi)
	end := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, nairobi).AddDate(0, 0, 1)
	if end.After(time.Now().Add(24 * time.Hour)) {
		return nil, apperr.Field("to", "cannot be in the future")
	}
	opening, err := wallet.BalanceAt(ctx, s.Pool, w.AvailableAccountID, start)
	if err != nil {
		return nil, err
	}
	closing, err := wallet.BalanceAt(ctx, s.Pool, w.AvailableAccountID, end)
	if err != nil {
		return nil, err
	}
	var lines []line
	var before *time.Time
	var beforeID *uuid.UUID
	for {
		page, err := wallet.ListActivity(ctx, s.Pool, w.ID, wallet.ActivityFilter{From: &start, To: &end, BeforeTime: before, BeforeID: beforeID, Limit: 100})
		if err != nil {
			return nil, err
		}
		for _, a := range page {
			l := line{At: a.CreatedAt.In(nairobi), Ref: a.ID.String(), Type: a.Type, Description: describe(a), Fee: a.Fee, Status: a.Status}
			if a.Reference != nil {
				l.Ref = *a.Reference
			}
			if a.Direction == "credit" {
				l.Credit = a.Amount
			} else {
				l.Debit = a.Amount
			}
			lines = append(lines, l)
		}
		if len(page) < 100 || len(lines) > 20_000 {
			break
		}
		last := page[len(page)-1]
		before, beforeID = &last.CreatedAt, &last.ID
	}
	// chronological order
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	var content []byte
	if format == "csv" {
		content, err = renderCSV(w, cur, start, end, opening, closing, lines)
	} else {
		content, err = renderPDF(w, cur, start, end, opening, closing, lines)
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(content)
	st := &Statement{ID: uuid.Must(uuid.NewV7()), WalletID: w.ID, PeriodStart: start.Format("2006-01-02"),
		PeriodEnd: end.AddDate(0, 0, -1).Format("2006-01-02"), Format: format, SHA256: hex.EncodeToString(sum[:]), SizeBytes: len(content)}
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO statements (id, user_id, wallet_id, period_start, period_end, format, content, sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`,
		st.ID, userID, w.ID, st.PeriodStart, st.PeriodEnd, format, content, st.SHA256).Scan(&st.CreatedAt)
	if err != nil {
		return nil, err
	}
	_ = audit.Record(ctx, s.Pool, audit.Event{Action: "statement.generated", TargetType: "statement", TargetID: st.ID.String()})
	return st, nil
}

func renderCSV(w wallet.Wallet, cur money.Currency, start, end time.Time, opening, closing int64, lines []line) ([]byte, error) {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{"# Wallet statement", w.AccountNumber, cur.Code, start.Format("2006-01-02"), end.AddDate(0, 0, -1).Format("2006-01-02")})
	_ = cw.Write([]string{"# Opening available balance", money.Format(opening, cur)})
	_ = cw.Write([]string{"date_eat", "reference", "type", "description", "debit", "credit", "fee", "status"})
	for _, l := range lines {
		debit, credit := "", ""
		if l.Debit > 0 {
			debit = money.Format(l.Debit, cur)
		}
		if l.Credit > 0 {
			credit = money.Format(l.Credit, cur)
		}
		_ = cw.Write([]string{l.At.Format("2006-01-02 15:04:05"), l.Ref, l.Type, sanitizeCSV(l.Description), debit, credit, money.Format(l.Fee, cur), l.Status})
	}
	_ = cw.Write([]string{"# Closing available balance", money.Format(closing, cur)})
	cw.Flush()
	return buf.Bytes(), cw.Error()
}

// sanitizeCSV neutralises spreadsheet formula injection.
func sanitizeCSV(s string) string {
	if s != "" && (s[0] == '=' || s[0] == '+' || s[0] == '-' || s[0] == '@') {
		return "'" + s
	}
	return s
}

func renderPDF(w wallet.Wallet, cur money.Currency, start, end time.Time, opening, closing int64, lines []line) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(12, 12, 12)
	pdf.SetAutoPageBreak(true, 14)
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	pdf.AddPage()
	pdf.SetFont("Helvetica", "B", 15)
	pdf.Cell(0, 8, "Wallet Statement")
	pdf.Ln(9)
	pdf.SetFont("Helvetica", "", 9)
	pdf.Cell(0, 5, fmt.Sprintf("Account %s  |  %s  |  %s to %s (EAT)", w.AccountNumber, cur.Code,
		start.Format("02 Jan 2006"), end.AddDate(0, 0, -1).Format("02 Jan 2006")))
	pdf.Ln(5)
	pdf.Cell(0, 5, fmt.Sprintf("Opening available balance: %s %s", cur.Code, money.Format(opening, cur)))
	pdf.Ln(8)
	headers := []string{"Date", "Description", "Debit", "Credit", "Fee", "Status"}
	widths := []float64{30, 72, 22, 22, 16, 24}
	pdf.SetFont("Helvetica", "B", 8)
	pdf.SetFillColor(10, 37, 64)
	pdf.SetTextColor(255, 255, 255)
	for i, h := range headers {
		pdf.CellFormat(widths[i], 6, h, "1", 0, "L", true, 0, "")
	}
	pdf.Ln(-1)
	pdf.SetTextColor(11, 28, 48)
	pdf.SetFont("Helvetica", "", 7.5)
	for _, l := range lines {
		desc := l.Description
		if len(desc) > 48 {
			desc = desc[:47] + "..."
		}
		debit, credit := "", ""
		if l.Debit > 0 {
			debit = money.Format(l.Debit, cur)
		}
		if l.Credit > 0 {
			credit = money.Format(l.Credit, cur)
		}
		cells := []string{l.At.Format("2006-01-02 15:04"), tr(desc), debit, credit, money.Format(l.Fee, cur), l.Status}
		for i, c := range cells {
			align := "L"
			if i >= 2 && i <= 4 {
				align = "R"
			}
			pdf.CellFormat(widths[i], 5.5, c, "1", 0, align, false, 0, "")
		}
		pdf.Ln(-1)
	}
	pdf.Ln(4)
	pdf.SetFont("Helvetica", "B", 9)
	pdf.Cell(0, 5, fmt.Sprintf("Closing available balance: %s %s", cur.Code, money.Format(closing, cur)))
	pdf.Ln(8)
	pdf.SetFont("Helvetica", "", 7)
	pdf.MultiCell(0, 4, "Balances are derived from the double-entry ledger. Pending and failed items are shown for reference and do not affect balances. Funds held for in-flight withdrawals are excluded from the available balance.", "", "L", false)
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Statement, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, wallet_id, period_start::text, period_end::text, format, sha256, octet_length(content), created_at
		  FROM statements WHERE user_id = $1 ORDER BY created_at DESC LIMIT 50`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Statement, error) {
		var st Statement
		err := r.Scan(&st.ID, &st.WalletID, &st.PeriodStart, &st.PeriodEnd, &st.Format, &st.SHA256, &st.SizeBytes, &st.CreatedAt)
		return st, err
	})
}

func (s *Service) Download(ctx context.Context, userID, id uuid.UUID) (string, []byte, error) {
	var format string
	var content []byte
	err := s.Pool.QueryRow(ctx, `SELECT format, content FROM statements WHERE id = $1 AND user_id = $2`, id, userID).Scan(&format, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, apperr.NotFound("statement")
	}
	return format, content, err
}
