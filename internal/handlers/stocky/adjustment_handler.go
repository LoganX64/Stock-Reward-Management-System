package stocky

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func (h *Handler) adjustmentHandler(c *gin.Context) {
	rewardID, ok := parseRewardID(c)
	if !ok {
		return
	}

	logger := logrus.WithFields(logrus.Fields{
		"request_id": requestID(c),
		"reward_id":  rewardID,
	})

	var req models.Adjustment
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.WithError(err).Warn("Invalid adjustment payload")
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("Invalid request payload"))
		return
	}

	// Validate adjustment type
	validTypes := map[string]bool{
		models.Reward_Reversal:   true,
		models.Fee_Refund:        true,
		models.Manual_Correction: true,
	}
	if _, ok := validTypes[req.AdjustmentType]; !ok {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("invalid adjustment type. must be one of: reward_reversal, fee_refund, manual_correction"))
		return
	}

	req.DeltaQuantity = utils.RoundQuantity(req.DeltaQuantity)
	req.DeltaAmount = utils.RoundAmount(req.DeltaAmount)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := h.DB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		logger.WithError(err).Error("Failed to begin transaction")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	// Ensure transaction is rolled back if not committed
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Fetch quantity AND stock_symbol in a SINGLE query and lock the reward row for update
	var currentQty float64
	var stockSymbol string
	err = tx.QueryRowContext(ctx, `
		SELECT quantity, stock_symbol 
		FROM rewards 
		WHERE id = $1
		FOR UPDATE
	`, rewardID).Scan(&currentQty, &stockSymbol)
	if err != nil {
		if err == sql.ErrNoRows {
			response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("reward not found"))
			return
		}
		logger.WithError(err).Error("Failed to fetch reward")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	// Prevent negative quantity
	if currentQty+req.DeltaQuantity < 0 {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("adjustment would make quantity negative"))
		return
	}

	// Insert adjustment
	var inserted models.Adjustment
	err = tx.QueryRowContext(ctx, `
		INSERT INTO adjustments (reward_id, adjustment_type, delta_quantity, delta_amount, reason, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		RETURNING id, reward_id, adjustment_type, delta_quantity, delta_amount, reason, created_at
	`,
		rewardID,
		req.AdjustmentType,
		req.DeltaQuantity,
		req.DeltaAmount,
		req.Reason).Scan(
		&inserted.ID,
		&inserted.RewardID,
		&inserted.AdjustmentType,
		&inserted.DeltaQuantity,
		&inserted.DeltaAmount,
		&inserted.Reason,
		&inserted.CreatedAt,
	)
	if err != nil {
		logger.WithError(err).Error("Failed to insert adjustment")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	// Update the main reward's quantity with a safety check to ensure the row remains non-negative
	res, err := tx.ExecContext(ctx, `
    UPDATE rewards 
    SET quantity = quantity + $1 
    WHERE id = $2
      AND quantity + $1 >= 0
`, req.DeltaQuantity, rewardID)

	if err != nil {
		logger.WithError(err).Error("Failed to update reward quantity")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		logger.WithError(err).Error("Failed to verify reward quantity update")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	if rowsAffected == 0 {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("adjustment would make quantity negative"))
		return
	}

	// Build ledger entries based on adjustment type
	ledgerEntries := []models.Ledger{}

	switch req.AdjustmentType {
	case models.Reward_Reversal:
		if req.DeltaQuantity != 0 {
			ledgerEntries = append(ledgerEntries, models.Ledger{
				Reward_ID:    rewardID,
				Entry_Type:   models.StockUnits,
				Stock_Symbol: stockSymbol,
				Quantity:     req.DeltaQuantity, // same signed delta as reward update
				Amount:       0,
			})
		}
		if req.DeltaAmount != 0 {
			ledgerEntries = append(ledgerEntries, models.Ledger{
				Reward_ID:  rewardID,
				Entry_Type: models.INROutflow,
				Amount:     -req.DeltaAmount, // usually money coming back
			})
		}
	case models.Fee_Refund:
		if req.DeltaAmount != 0 {
			ledgerEntries = append(ledgerEntries, models.Ledger{
				Reward_ID:  rewardID,
				Entry_Type: models.INROutflow,
				Amount:     req.DeltaAmount, // money refunded
			})
		}
	case models.Manual_Correction:
		if req.DeltaQuantity != 0 {
			ledgerEntries = append(ledgerEntries, models.Ledger{
				Reward_ID:    rewardID,
				Entry_Type:   models.StockUnits,
				Stock_Symbol: stockSymbol,
				Quantity:     req.DeltaQuantity,
				Amount:       0,
			})
		}
		if req.DeltaAmount != 0 {
			ledgerEntries = append(ledgerEntries, models.Ledger{
				Reward_ID:  rewardID,
				Entry_Type: models.INROutflow,
				Amount:     req.DeltaAmount, // cash correction
			})
		}
	}

	// Insert ledger entries
	for _, entry := range ledgerEntries {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ledger (reward_id, entry_type, stock_symbol, quantity, amount, created_at)
			VALUES ($1,$2,$3,$4,$5,NOW())
		`,
			entry.Reward_ID,
			entry.Entry_Type,
			entry.Stock_Symbol,
			utils.RoundQuantity(entry.Quantity),
			utils.RoundAmount(entry.Amount)); err != nil {
			logger.WithError(err).Error("Failed to insert ledger entry")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("failed to update ledger"))
			return
		}
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		logger.WithError(err).Error("Failed to commit transaction")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	committed = true

	logger.WithFields(logrus.Fields{
		"adjustment_id": inserted.ID,
		"delta_qty":     req.DeltaQuantity,
		"delta_amount":  req.DeltaAmount,
	}).Info("Adjustment applied successfully")

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"message":     "Adjustment applied successfully",
		"rewardId":    rewardID,
		"stockSymbol": stockSymbol,
		"data":        inserted,
	})
}
