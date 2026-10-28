package stocky

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
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
	if req.DeltaQuantity == 0 && req.DeltaAmount == 0 {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("adjustment must change quantity or amount"))
		return
	}
	switch req.AdjustmentType {
	case models.Reward_Reversal:
		if req.DeltaQuantity > 0 || req.DeltaAmount < 0 {
			response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("reward_reversal requires non-positive delta_quantity and non-negative delta_amount"))
			return
		}
	case models.Fee_Refund:
		if req.DeltaQuantity != 0 || req.DeltaAmount < 0 {
			response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("fee_refund requires zero delta_quantity and non-negative delta_amount"))
			return
		}
	}

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

	// Fetch the reward at its reward-date-equivalent balance and lock it for update.
	// The requested delta is denominated in units on the adjustment effective date.
	var currentQty float64
	var stockSymbol string
	var normalizationFactor float64
	err = tx.QueryRowContext(ctx, `
		SELECT
			r.stock_symbol,
			COALESCE((
				SELECT SUM(m.normalized_quantity_delta)
				FROM reward_quantity_movements m
				WHERE m.reward_id = r.id
			), 0) AS normalized_quantity,
			stock_quantity_factor(r.stock_symbol, r.created_at::date)
				/ stock_quantity_factor(r.stock_symbol, CURRENT_DATE) AS normalization_factor
		FROM rewards r
		WHERE r.id = $1
		FOR UPDATE
	`, rewardID).Scan(&stockSymbol, &currentQty, &normalizationFactor)
	if err != nil {
		if err == sql.ErrNoRows {
			response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("reward not found"))
			return
		}
		logger.WithError(err).Error("Failed to fetch reward")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	normalizedDelta := req.DeltaQuantity * normalizationFactor
	newQty := currentQty + normalizedDelta
	if newQty < 0 {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("adjustment would make quantity negative"))
		return
	}

	// Keep the insert in a savepoint so a concurrent duplicate key can be
	// resolved and replayed without aborting the surrounding transaction.
	if req.IdempotencyKey != "" {
		if _, err := tx.ExecContext(ctx, `SAVEPOINT adjustment_insert`); err != nil {
			logger.WithError(err).Error("Failed to create adjustment insert savepoint")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
			return
		}
	}

	// Insert adjustment
	var inserted models.Adjustment
	err = tx.QueryRowContext(ctx, `
		INSERT INTO adjustments (reward_id, adjustment_type, delta_quantity, delta_amount, reason, idempotency_key, effective_date, created_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), CURRENT_DATE, NOW())
		RETURNING id, reward_id, adjustment_type, delta_quantity, delta_amount, reason, COALESCE(idempotency_key, ''), effective_date::text, created_at
	`,
		rewardID,
		req.AdjustmentType,
		req.DeltaQuantity,
		req.DeltaAmount,
		req.Reason,
		req.IdempotencyKey).Scan(
		&inserted.ID,
		&inserted.RewardID,
		&inserted.AdjustmentType,
		&inserted.DeltaQuantity,
		&inserted.DeltaAmount,
		&inserted.Reason,
		&inserted.IdempotencyKey,
		&inserted.EffectiveDate,
		&inserted.CreatedAt,
	)
	if err != nil {
		if req.IdempotencyKey != "" {
			if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
				if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT adjustment_insert`); rollbackErr != nil {
					logger.WithError(rollbackErr).Error("Failed to roll back adjustment insert savepoint")
					response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
					return
				}
				var existing models.Adjustment
				var storedResponse []byte
				err = tx.QueryRowContext(ctx, `
					SELECT id, reward_id, adjustment_type, delta_quantity, delta_amount, COALESCE(reason, ''),
					       COALESCE(idempotency_key, ''), effective_date::text, created_at, original_response
					FROM adjustments WHERE idempotency_key = $1
				`, req.IdempotencyKey).Scan(
					&existing.ID, &existing.RewardID, &existing.AdjustmentType,
					&existing.DeltaQuantity, &existing.DeltaAmount, &existing.Reason,
					&existing.IdempotencyKey, &existing.EffectiveDate, &existing.CreatedAt, &storedResponse,
				)
				if err != nil {
					logger.WithError(err).Error("Failed to fetch existing adjustment")
					response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
					return
				}
				if existing.RewardID != rewardID || existing.AdjustmentType != req.AdjustmentType ||
					existing.DeltaQuantity != req.DeltaQuantity || existing.DeltaAmount != req.DeltaAmount || existing.Reason != req.Reason {
					response.WriteJson(c.Writer, http.StatusConflict, response.ErrorResponse("idempotency key already used with different payload"))
					return
				}
				var replay interface{}
				if len(storedResponse) == 0 || json.Unmarshal(storedResponse, &replay) != nil {
					logger.Error("Stored adjustment response is invalid")
					response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
					return
				}
				if err := tx.Commit(); err != nil {
					logger.WithError(err).Error("Failed to commit retry transaction")
					response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
					return
				}
				committed = true
				response.WriteJson(c.Writer, http.StatusOK, replay)
				return
			}
		}
		logger.WithError(err).Error("Failed to insert adjustment")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	// Store the new reward-date-equivalent balance with a non-negative guard.
	res, err := tx.ExecContext(ctx, `
    UPDATE rewards
    SET quantity = $1
    WHERE id = $2
      AND $1 >= 0
`, newQty, rewardID)

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
				Amount:     req.DeltaAmount, // money returned
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

	responseBody := map[string]interface{}{
		"message":     "Adjustment applied successfully",
		"rewardId":    rewardID,
		"stockSymbol": stockSymbol,
		"data":        inserted,
	}
	responseJSON, err := json.Marshal(responseBody)
	if err != nil {
		logger.WithError(err).Error("Failed to encode adjustment response")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE adjustments SET original_response = $1 WHERE id = $2`, responseJSON, inserted.ID); err != nil {
		logger.WithError(err).Error("Failed to store original adjustment response")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
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

	response.WriteJson(c.Writer, http.StatusOK, responseBody)
}
