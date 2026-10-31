package stocky

import (
	"context"
	"database/sql"
	"time"

	"github.com/LoganX64/stocky-api/internal/marketcal"
	"github.com/shopspring/decimal"
)

// Handler holds all dependencies (mainly DB)
type Handler struct {
	DB *sql.DB

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

// portfolioFreshness is the pagination-independent staleness summary for a
// user's full current portfolio.
type portfolioFreshness struct {
	StalePositions int
	StaleSymbols   []string
	PricesFresh    bool
}

// getPortfolioFreshness classifies every current holding for the user, not just
// the requested page. That keeps stalePositions and staleSymbols correct when
// /portfolio is paginated.
func (h *Handler) getPortfolioFreshness(q dbReader, userID int) (portfolioFreshness, error) {
	rows, err := q.Query(`
		SELECT stock_symbol, price_status, price_as_of, price_source
		FROM user_portfolio
		WHERE user_id = $1
		ORDER BY stock_symbol
	`, userID)
	if err != nil {
		return portfolioFreshness{}, err
	}
	defer rows.Close()

	tally := newFreshnessTally()
	for rows.Next() {
		var symbol, priceStatus string
		var priceAsOf sql.NullTime
		var priceSource sql.NullString
		if err := rows.Scan(&symbol, &priceStatus, &priceAsOf, &priceSource); err != nil {
			return portfolioFreshness{}, err
		}
		tally.add(symbol, priceStatus, h.classifyPriceFreshness(priceStatus, priceAsOf, priceSource))
	}
	if err := rows.Err(); err != nil {
		return portfolioFreshness{}, err
	}

	return portfolioFreshness{
		StalePositions: tally.stale,
		StaleSymbols:   tally.symbols,
		PricesFresh:    tally.fresh,
	}, nil
}

// dbReader is implemented by both *sql.DB and *sql.Tx. Reporting helpers use it
// so they can run inside the same read-only snapshot transaction as their
// handler.
type dbReader interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// beginReadSnapshot starts a read-only repeatable-read transaction. Every read
// in a reporting response then shares one MVCC snapshot and one market date,
// which keeps position sets and totals consistent.
func (h *Handler) beginReadSnapshot(ctx context.Context) (*sql.Tx, error) {
	return h.DB.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
}

// getUserCorporateActionCash returns the user's total cash proceeds,
// written-off quantity, and the cash-in-lieu subset of total cash proceeds.
func (h *Handler) getUserCorporateActionCash(q dbReader, userID int) (decimal.Decimal, decimal.Decimal, decimal.Decimal, error) {
	var cashProceeds, writtenOffQuantity, cashInLieuProceeds decimal.Decimal
	err := q.QueryRow(`
		SELECT cash_proceeds, written_off_quantity, cash_in_lieu_proceeds
		FROM user_corporate_action_cash
		WHERE user_id = $1
	`, userID).Scan(&cashProceeds, &writtenOffQuantity, &cashInLieuProceeds)
	if err == sql.ErrNoRows {
		return decimal.Zero, decimal.Zero, decimal.Zero, nil
	}
	return cashProceeds, writtenOffQuantity, cashInLieuProceeds, err
}

// getUnpricedPortfolioSymbols returns the symbols in the user's active
// portfolio that have no current price. A holding with a missing price is still
// returned by user_portfolio, but its current_price and inr_value are NULL.
func (h *Handler) getUnpricedPortfolioSymbols(q dbReader, userID int) ([]string, error) {
	rows, err := q.Query(`
		SELECT stock_symbol
		FROM user_portfolio
		WHERE user_id = $1
		  AND current_price IS NULL
		ORDER BY stock_symbol
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	symbols := []string{}
	for rows.Next() {
		var symbol string
		if err := rows.Scan(&symbol); err != nil {
			return nil, err
		}
		symbols = append(symbols, symbol)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return symbols, nil
}

// getPortfolioTotals returns full-set totals for a user's active holdings,
// independent of pagination. totalPortfolioValue sums only priced holdings;
// totalPositions includes unpriced holdings. valuationDate is the database
// date used by the shared reporting views in the current snapshot.
func (h *Handler) getPortfolioTotals(q dbReader, userID int) (decimal.Decimal, int, string, error) {
	var totalPortfolioValue decimal.Decimal
	var totalPositions int
	var valuationDate string

	err := q.QueryRow(`
		SELECT
			COALESCE(SUM(inr_value), 0),
			COUNT(*),
			market_today()::TEXT
		FROM user_portfolio
		WHERE user_id = $1
	`, userID).Scan(&totalPortfolioValue, &totalPositions, &valuationDate)
	if err != nil {
		return decimal.Zero, 0, "", err
	}
	return totalPortfolioValue, totalPositions, valuationDate, nil
}

// getMarketToday returns the market (IST) date used by the reporting views. It
// is read inside the same snapshot as the rest of the response so a request
// never mixes two valuation dates.
func (h *Handler) getMarketToday(q dbReader) (string, error) {
	var marketToday string
	err := q.QueryRow(`SELECT market_today()::TEXT`).Scan(&marketToday)
	return marketToday, err
}

// getUnpricedHistoricalRewards counts historical reward rows that have no
// price and returns the distinct affected symbols. Rows with a zero effective
// quantity report price_status = 'not_required' and are not gaps. When asOf is
// nil the grant-date mode and its "before today" filter are used; otherwise the
// same as-of date is used for the page and this full-set consistency check.
func (h *Handler) getUnpricedHistoricalRewards(q dbReader, userID int, asOf *string) (int, []string, error) {
	var asOfArg any
	if asOf != nil {
		asOfArg = *asOf
	}

	rows, err := q.Query(`
		SELECT stock_symbol
		FROM historical_rewards_as_of($2::date)
		WHERE user_id = $1
		  AND reward_date <= COALESCE($2::date, market_today() - 1)
		  AND price_status = 'missing'
		ORDER BY stock_symbol
	`, userID, asOfArg)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	count := 0
	seen := map[string]struct{}{}
	symbols := []string{}
	for rows.Next() {
		var symbol string
		if err := rows.Scan(&symbol); err != nil {
			return 0, nil, err
		}
		count++
		if _, ok := seen[symbol]; !ok {
			seen[symbol] = struct{}{}
			symbols = append(symbols, symbol)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	return count, symbols, nil
}
