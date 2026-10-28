// historical_handler.go
package stocky

import (
	"database/sql"
	"net/http"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func (h *Handler) GetHistoricalINR(c *gin.Context) {
	userID, ok := parseUserID(c)
	if !ok {
		return
	}
	limit, offset, ok := parsePagination(c)
	if !ok {
		return
	}

	logger := logrus.WithFields(logrus.Fields{
		"request_id": requestID(c),
		"user_id":    userID,
	})

	rows, err := h.DB.Query(`
		SELECT 
			reward_date, 
			reward_event_id, 
			stock_symbol,
			adjusted_quantity, 
			price, 
			total_adjustment_amount, 
			inr_value,
			price_status,
			price_as_of
		FROM historical_rewards
		WHERE user_id = $1
		  AND reward_date < CURRENT_DATE
		ORDER BY reward_date DESC, reward_event_id DESC
		LIMIT $2 OFFSET $3
	`, userID, limit, offset)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch historical INR")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	defer rows.Close()

	var history []models.HistoricalINR
	for rows.Next() {
		var rec models.HistoricalINR
		var price sql.NullFloat64
		var inrValue sql.NullFloat64
		var priceAsOf sql.NullTime
		if err := rows.Scan(
			&rec.RewardDate,
			&rec.RewardEventID,
			&rec.StockSymbol,
			&rec.AdjustedQuantity,
			&price,
			&rec.TotalAdjustmentAmount,
			&inrValue,
			&rec.PriceStatus,
			&priceAsOf,
		); err != nil {
			logger.WithError(err).Error("Failed to scan historical INR record")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
			return
		}

		// Apply consistent rounding to available values and preserve NULL for
		// missing reward-date prices.
		rec.AdjustedQuantity = utils.RoundQuantity(rec.AdjustedQuantity)
		rec.TotalAdjustmentAmount = utils.RoundAmount(rec.TotalAdjustmentAmount)
		if price.Valid {
			v := utils.RoundAmount(price.Float64)
			rec.Price = &v
		}
		if inrValue.Valid {
			v := utils.RoundAmount(inrValue.Float64)
			rec.INRValue = &v
		}
		if priceAsOf.Valid {
			t := priceAsOf.Time
			rec.PriceAsOf = &t
		}

		history = append(history, rec)
	}

	if err := rows.Err(); err != nil {
		logger.WithError(err).Error("Error iterating historical INR rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	if err := rows.Close(); err != nil {
		logger.WithError(err).Error("Failed to close historical INR rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	unpricedRewards, unpricedSymbols, err := h.getUnpricedHistoricalRewards(userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch unpriced historical rewards")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":            userID,
		"limit":             limit,
		"offset":            offset,
		"history":           utils.OrEmpty(history),
		"valuationComplete": unpricedRewards == 0,
		"unpricedRewards":   unpricedRewards,
		"unpricedSymbols":   utils.OrEmpty(unpricedSymbols),
	})
}
