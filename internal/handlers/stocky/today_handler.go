package stocky

import (
	"database/sql"
	"net/http"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
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
			price_as_of,
			price_source,
			price_fetched_at
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
	freshness := newFreshnessTally()
	for rows.Next() {
		var s models.TodayStock
		var currentPrice decimal.NullDecimal
		var inrValue decimal.NullDecimal
		var priceAsOf sql.NullTime
		var priceSource sql.NullString
		var priceFetchedAt sql.NullTime
		if err := rows.Scan(
			&s.RewardID,
			&s.StockSymbol,
			&s.AdjustedQuantity,
			&currentPrice,
			&s.TotalAdjustmentAmount,
			&inrValue,
			&s.PriceStatus,
			&priceAsOf,
			&priceSource,
			&priceFetchedAt,
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
			s.CurrentPrice = decimal.NullDecimal{Decimal: utils.RoundAmount(currentPrice.Decimal), Valid: true}
		} else {
			unpricedRewards++
			unpricedSymbols = append(unpricedSymbols, s.StockSymbol)
		}
		if inrValue.Valid {
			s.INRValue = decimal.NullDecimal{Decimal: utils.RoundAmount(inrValue.Decimal), Valid: true}
		}
		if priceAsOf.Valid {
			t := priceAsOf.Time.UTC()
			s.PriceAsOf = &t
		}
		if priceSource.Valid {
			source := priceSource.String
			s.PriceSource = &source
		}
		if priceFetchedAt.Valid {
			t := priceFetchedAt.Time.UTC()
			s.PriceFetchedAt = &t
		}
		s.PriceFreshness = h.classifyPriceFreshness(s.PriceStatus, priceAsOf, priceSource)
		freshness.add(s.StockSymbol, s.PriceStatus, s.PriceFreshness)

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
		"staleRewards":      freshness.stale,
		"staleSymbols":      utils.OrEmpty(freshness.symbols),
		"pricesFresh":       freshness.fresh,
	})
}
