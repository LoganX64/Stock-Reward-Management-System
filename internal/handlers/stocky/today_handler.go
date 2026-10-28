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

func (h *Handler) GetTodayStocks(c *gin.Context) {
	userID, ok := parseUserID(c)
	if !ok {
		return
	}
	logger := logrus.WithFields(logrus.Fields{
		"request_id": requestID(c),
		"user_id":    userID,
	})

	rows, err := h.DB.Query(`
		SELECT 
			reward_event_id,
			stock_symbol,
			adjusted_quantity,
			current_price,
			total_adjustment_amount,
			inr_value,
			price_status,
			price_as_of
		FROM today_rewards
		WHERE user_id = $1
		ORDER BY stock_symbol, reward_event_id
	`, userID)

	if err != nil {
		logger.WithError(err).Error("Failed to fetch today stocks")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}
	defer rows.Close()

	var stocks []models.TodayStock
	unpricedRewards := 0
	unpricedSymbols := []string{}
	for rows.Next() {
		var s models.TodayStock
		var currentPrice sql.NullFloat64
		var inrValue sql.NullFloat64
		var priceAsOf sql.NullTime
		if err := rows.Scan(
			&s.RewardID,
			&s.StockSymbol,
			&s.AdjustedQuantity,
			&currentPrice,
			&s.TotalAdjustmentAmount,
			&inrValue,
			&s.PriceStatus,
			&priceAsOf,
		); err != nil {
			logger.WithError(err).Error("scan error")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
			return
		}

		// Apply consistent rounding to available values and preserve NULL for
		// missing prices.
		s.AdjustedQuantity = utils.RoundQuantity(s.AdjustedQuantity)
		s.TotalAdjustmentAmount = utils.RoundAmount(s.TotalAdjustmentAmount)
		if currentPrice.Valid {
			v := utils.RoundAmount(currentPrice.Float64)
			s.CurrentPrice = &v
		} else {
			unpricedRewards++
			unpricedSymbols = append(unpricedSymbols, s.StockSymbol)
		}
		if inrValue.Valid {
			v := utils.RoundAmount(inrValue.Float64)
			s.INRValue = &v
		}
		if priceAsOf.Valid {
			t := priceAsOf.Time
			s.PriceAsOf = &t
		}

		stocks = append(stocks, s)
	}
	if err := rows.Err(); err != nil {
		logger.WithError(err).Error("Error iterating today stocks rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("internal server error"))
		return
	}

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":            userID,
		"stocks":            utils.OrEmpty(stocks),
		"valuationComplete": unpricedRewards == 0,
		"unpricedRewards":   unpricedRewards,
		"unpricedSymbols":   utils.OrEmpty(unpricedSymbols),
	})
}
