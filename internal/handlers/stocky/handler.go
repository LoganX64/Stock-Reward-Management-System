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

// getUserCorporateActionCash returns the user's total cash proceeds and
// written-off quantity from terminal merger/delisting outcomes.
func (h *Handler) getUserCorporateActionCash(userID int) (float64, float64, error) {
	var cashProceeds, writtenOffQuantity float64
	err := h.DB.QueryRow(`
		SELECT cash_proceeds, written_off_quantity
		FROM user_corporate_action_cash
		WHERE user_id = $1
	`, userID).Scan(&cashProceeds, &writtenOffQuantity)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	return cashProceeds, writtenOffQuantity, err
}
