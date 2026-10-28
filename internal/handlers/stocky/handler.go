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
