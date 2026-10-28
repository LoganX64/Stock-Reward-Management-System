package stocky

import (
	"context"
	"database/sql"
)

// Handler holds all dependencies (mainly DB)
type Handler struct {
	DB *sql.DB
}

// NewHandler creates a new Handler instance with injected dependencies
func NewHandler(db *sql.DB) *Handler {
	return &Handler{
		DB: db,
	}
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
func (h *Handler) getUserCorporateActionCash(q dbReader, userID int) (float64, float64, float64, error) {
	var cashProceeds, writtenOffQuantity, cashInLieuProceeds float64
	err := q.QueryRow(`
		SELECT cash_proceeds, written_off_quantity, cash_in_lieu_proceeds
		FROM user_corporate_action_cash
		WHERE user_id = $1
	`, userID).Scan(&cashProceeds, &writtenOffQuantity, &cashInLieuProceeds)
	if err == sql.ErrNoRows {
		return 0, 0, 0, nil
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
func (h *Handler) getPortfolioTotals(q dbReader, userID int) (float64, int, string, error) {
	var totalPortfolioValue float64
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
		return 0, 0, "", err
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
