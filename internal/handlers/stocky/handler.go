package stocky

import (
	"context"
	"database/sql"
	"time"

	"github.com/LoganX64/stocky-api/internal/marketcal"
	"github.com/LoganX64/stocky-api/internal/metrics"
	"github.com/shopspring/decimal"
)

// Handler holds all dependencies (mainly DB)
type Handler struct {
	DB *sql.DB

	// ReadDB is an optional read-replica pool. When nil, reads use DB, which
	// preserves read-your-writes for single-database deployments.
	ReadDB *sql.DB

	// Metrics is optional so tests and small deployments can omit it.
	Metrics *metrics.Metrics

	// Calendar and Freshness drive the calendar-aware staleness reported for
	// current prices. They default to the NSE calendar and the shared A3
	// policy so the API and the scheduler classify quotes identically.
	Calendar  *marketcal.Calendar
	Freshness marketcal.FreshnessPolicy

	// Now is the clock used for freshness classification. It is injectable so
	// tests and replays can produce deterministic freshness values.
	Now func() time.Time
}

// Option customises a Handler without breaking existing NewHandler(db) calls.
type Option func(*Handler)

// WithCalendar sets the market calendar used for freshness classification.
func WithCalendar(calendar *marketcal.Calendar) Option {
	return func(h *Handler) {
		if calendar == nil {
			return
		}
		h.Calendar = calendar
		h.Freshness.Calendar = calendar
	}
}

// WithFreshness sets the freshness policy used for API responses.
func WithFreshness(policy marketcal.FreshnessPolicy) Option {
	return func(h *Handler) {
		h.Freshness = policy
		if h.Freshness.Calendar == nil {
			h.Freshness.Calendar = h.Calendar
		}
	}
}

// WithClock overrides the clock used for freshness classification.
func WithClock(now func() time.Time) Option {
	return func(h *Handler) {
		if now != nil {
			h.Now = now
		}
	}
}

// WithReadDB routes reporting reads through a read replica. Writes always stay
// on the primary DB.
func WithReadDB(db *sql.DB) Option {
	return func(h *Handler) {
		if db != nil {
			h.ReadDB = db
		}
	}
}

// WithMetrics attaches DB and projection metrics to the handler.
func WithMetrics(m *metrics.Metrics) Option {
	return func(h *Handler) {
		if m != nil {
			h.Metrics = m
		}
	}
}

// NewHandler creates a new Handler instance with injected dependencies
func NewHandler(db *sql.DB, opts ...Option) *Handler {
	calendar := marketcal.Default()
	h := &Handler{
		DB:        db,
		Calendar:  calendar,
		Freshness: marketcal.DefaultFreshnessPolicy(calendar),
		Now:       time.Now,
	}
	for _, opt := range opts {
		opt(h)
	}
	if h.Freshness.Calendar == nil {
		h.Freshness.Calendar = h.Calendar
	}
	return h
}

// classifyPriceFreshness returns the freshness of a current-price row. Missing
// and not-required rows have no freshness. An available row without a usable
// timestamp, or with legacy/seed provenance, is explicitly "unknown" rather
// than being reported as fresh.
func (h *Handler) classifyPriceFreshness(priceStatus string, asOf sql.NullTime, source sql.NullString) *string {
	if priceStatus != "available" {
		return nil
	}

	freshness := marketcal.FreshnessUnknown
	if asOf.Valid {
		quoteSource := ""
		if source.Valid {
			quoteSource = source.String
		}
		freshness = h.Freshness.ClassifySource(quoteSource, asOf.Time, h.Now())
	}

	value := string(freshness)
	return &value
}

// freshnessTally aggregates staleness over a set of current-price rows.
// Missing rows are a valuation gap reported elsewhere, so they neither count as
// stale nor make pricesFresh false. An unknown classification is not stale but
// does make pricesFresh false.
type freshnessTally struct {
	stale   int
	symbols []string
	seen    map[string]struct{}
	fresh   bool
}

func newFreshnessTally() *freshnessTally {
	return &freshnessTally{
		seen:  map[string]struct{}{},
		fresh: true,
	}
}

func (t *freshnessTally) add(symbol, priceStatus string, freshness *string) {
	if priceStatus != "available" {
		return
	}
	if freshness == nil || *freshness != string(marketcal.FreshnessFresh) {
		t.fresh = false
	}
	if freshness == nil || *freshness != string(marketcal.FreshnessStale) {
		return
	}

	t.stale++
	if _, ok := t.seen[symbol]; ok {
		return
	}
	t.seen[symbol] = struct{}{}
	t.symbols = append(t.symbols, symbol)
}

// portfolioSummary is the pagination-independent summary for a user's current
// portfolio. One full-set query provides the totals, unpriced symbols,
// calendar-aware freshness, and corporate-action cash so /portfolio and /stats
// do not re-scan user_portfolio for each field.
type portfolioSummary struct {
	TotalPortfolioValue decimal.Decimal
	TotalPositions      int
	ValuationDate       string
	UnpricedSymbols     []string
	StalePositions      int
	StaleSymbols        []string
	PricesFresh         bool
	CashProceeds        decimal.Decimal
	WrittenOffQuantity  decimal.Decimal
	CashInLieuProceeds  decimal.Decimal
}

// dbReader is implemented by both *sql.DB and *sql.Tx. Reporting helpers use it
// so they can run inside the same read-only snapshot transaction as their
// handler. Context-aware methods make request deadlines reach PostgreSQL.
type dbReader interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readDB returns the configured read replica, or the primary pool when no
// replica is configured.
func (h *Handler) readDB() *sql.DB {
	if h.ReadDB != nil {
		return h.ReadDB
	}
	return h.DB
}

// beginReadSnapshot starts a read-only repeatable-read transaction. Every read
// in a reporting response then shares one MVCC snapshot and one market date,
// which keeps position sets and totals consistent.
func (h *Handler) beginReadSnapshot(ctx context.Context) (*sql.Tx, error) {
	return h.readDB().BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
}

// getPortfolioSummary reads every current holding for the user once and
// aggregates full-set totals, valuation gaps, freshness, and corporate-action
// cash. The anchor row guarantees a result even for a user with no portfolio or
// cash position, so valuationDate is always available.
func (h *Handler) getPortfolioSummary(ctx context.Context, q dbReader, userID int) (portfolioSummary, error) {
	started := time.Now()
	rows, err := q.QueryContext(ctx, `
		SELECT
			p.stock_symbol,
			p.price_status,
			p.price_as_of,
			p.price_source,
			p.current_price,
			p.inr_value,
			c.cash_proceeds,
			c.written_off_quantity,
			c.cash_in_lieu_proceeds,
			market_today()::TEXT AS valuation_date
		FROM (SELECT 1) AS anchor
		LEFT JOIN user_portfolio p
			ON p.user_id = $1
		LEFT JOIN user_corporate_action_cash c
			ON c.user_id = $1
		ORDER BY p.stock_symbol
	`, userID)
	h.Metrics.ObserveQuery("portfolio_summary", started, err)
	if err != nil {
		return portfolioSummary{}, err
	}
	defer rows.Close()

	summary := portfolioSummary{
		TotalPortfolioValue: decimal.Zero,
		UnpricedSymbols:     []string{},
		StaleSymbols:        []string{},
		PricesFresh:         true,
	}
	tally := newFreshnessTally()

	for rows.Next() {
		var symbol sql.NullString
		var priceStatus sql.NullString
		var priceAsOf sql.NullTime
		var priceSource sql.NullString
		var currentPrice decimal.NullDecimal
		var inrValue decimal.NullDecimal
		var cashProceeds decimal.NullDecimal
		var writtenOffQuantity decimal.NullDecimal
		var cashInLieuProceeds decimal.NullDecimal
		var valuationDate string

		if err := rows.Scan(
			&symbol,
			&priceStatus,
			&priceAsOf,
			&priceSource,
			&currentPrice,
			&inrValue,
			&cashProceeds,
			&writtenOffQuantity,
			&cashInLieuProceeds,
			&valuationDate,
		); err != nil {
			return portfolioSummary{}, err
		}

		summary.ValuationDate = valuationDate
		if cashProceeds.Valid {
			summary.CashProceeds = cashProceeds.Decimal
		}
		if writtenOffQuantity.Valid {
			summary.WrittenOffQuantity = writtenOffQuantity.Decimal
		}
		if cashInLieuProceeds.Valid {
			summary.CashInLieuProceeds = cashInLieuProceeds.Decimal
		}

		// The anchor row has no position; it still carries the valuation date
		// and the cash outcome for users with no holdings.
		if !symbol.Valid {
			continue
		}

		summary.TotalPositions++
		if inrValue.Valid {
			summary.TotalPortfolioValue = summary.TotalPortfolioValue.Add(inrValue.Decimal)
		}
		if !currentPrice.Valid {
			summary.UnpricedSymbols = append(summary.UnpricedSymbols, symbol.String)
		}
		tally.add(symbol.String, priceStatus.String, h.classifyPriceFreshness(priceStatus.String, priceAsOf, priceSource))
	}
	if err := rows.Err(); err != nil {
		return portfolioSummary{}, err
	}

	summary.StalePositions = tally.stale
	if tally.symbols != nil {
		summary.StaleSymbols = tally.symbols
	}
	summary.PricesFresh = tally.fresh
	return summary, nil
}
