package stocky

import (
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

// getUserCorporateActionCash returns the user's total cash proceeds,
// written-off quantity, and the cash-in-lieu subset of total cash proceeds.
func (h *Handler) getUserCorporateActionCash(userID int) (float64, float64, float64, error) {
	var cashProceeds, writtenOffQuantity, cashInLieuProceeds float64
	err := h.DB.QueryRow(`
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
func (h *Handler) getUnpricedPortfolioSymbols(userID int) ([]string, error) {
	rows, err := h.DB.Query(`
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

// getUnpricedHistoricalRewards counts historical reward rows that have no
// reward-date price and returns the distinct affected symbols.
func (h *Handler) getUnpricedHistoricalRewards(userID int) (int, []string, error) {
	rows, err := h.DB.Query(`
		SELECT stock_symbol
		FROM historical_rewards
		WHERE user_id = $1
		  AND reward_date < CURRENT_DATE
		  AND price IS NULL
		ORDER BY stock_symbol
	`, userID)
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
