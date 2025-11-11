package stocky

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func adjustmentHandler(c *gin.Context) {
	idParam := c.Param("id")
	rewardID, err := strconv.Atoi(idParam)
	if err != nil {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("invalid reward ID"))
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

	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		logger.WithError(err).Error("Failed to begin transaction")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	rolledBack := false
	defer func() {
		if !rolledBack {
			_ = tx.Rollback()
		}
	}()

	var currentQty float64
	err = tx.QueryRowContext(ctx, `
		SELECT quantity FROM rewards WHERE id=$1
	`, rewardID).Scan(&currentQty)
	if err != nil {
		if err == sql.ErrNoRows {
			response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("reward not found"))
			return
		}
		logger.WithError(err).Error("Failed to fetch reward")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	var totalDeltaQty float64
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(delta_quantity),0) FROM adjustments WHERE reward_id=$1
	`, rewardID).Scan(&totalDeltaQty)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch adjustment sum")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	if currentQty+totalDeltaQty+req.DeltaQuantity < 0 {
		response.WriteJson(c.Writer, http.StatusBadRequest, response.ErrorResponse("adjustment would make quantity negative"))
		return
	}

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

	var stockSymbol string
	err = tx.QueryRowContext(ctx, `SELECT stock_symbol FROM rewards WHERE id=$1`, rewardID).Scan(&stockSymbol)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch reward stock symbol")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	ledgerEntries := []models.Ledger{}

	switch req.AdjustmentType {
	case models.Reward_Reversal:
		if req.DeltaQuantity != 0 {
			ledgerEntries = append(ledgerEntries, models.Ledger{
				Reward_ID:    rewardID,
				Entry_Type:   models.StockUnits,
				Stock_Symbol: stockSymbol,
				Quantity:     -req.DeltaQuantity,
				Amount:       0,
			})
		}
		if req.DeltaAmount != 0 {
			ledgerEntries = append(ledgerEntries, models.Ledger{
				Reward_ID:  rewardID,
				Entry_Type: models.INROutflow,
				Amount:     -req.DeltaAmount,
			})
		}
	case models.Fee_Refund:
		if req.DeltaAmount != 0 {
			ledgerEntries = append(ledgerEntries, models.Ledger{
				Reward_ID:  rewardID,
				Entry_Type: models.INROutflow,
				Amount:     req.DeltaAmount,
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
				Amount:     req.DeltaAmount,
			})
		}
	}

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

	if err := tx.Commit(); err != nil {
		logger.WithError(err).Error("Failed to commit transaction")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	rolledBack = true

	logger.WithFields(logrus.Fields{
		"adjustment_id": inserted.ID,
	}).Info("Adjustment applied successfully")

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"message":  "Adjustment applied successfully",
		"rewardId": rewardID,
		"data":     inserted,
	})
}
