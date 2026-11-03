package stocky

import (
	"database/sql"
	"net/http"
	"time"

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

	started := time.Now()
	rows, err := h.readDB().QueryContext(c.Request.Context(), `
		SELECT
			rs.reward_id AS reward_event_id,
			rs.terminal_symbol AS stock_symbol,
			rs.adjusted_quantity::NUMERIC AS adjusted_quantity,
			spa.adjusted_price AS current_price,
			rs.total_adjustment_amount::NUMERIC AS total_adjustment_amount,
			CASE
				WHEN spa.adjusted_price IS NULL THEN NULL::NUMERIC
				ELSE ROUND(rs.adjusted_quantity * spa.adjusted_price, 4)
			END AS inr_value,
			CASE
				WHEN spa.adjusted_price IS NULL THEN 'missing'
				ELSE 'available'
			END AS price_status,
			spa.updated_at AS price_as_of,
			spa.source AS price_source,
			spa.fetched_at AS price_fetched_at
		FROM reward_state rs
		LEFT JOIN stock_prices_adjusted spa
			ON spa.stock_symbol = rs.terminal_symbol
		WHERE rs.user_id = $1
		  AND rs.reward_date = market_today()
		  AND rs.is_active
		ORDER BY rs.terminal_symbol, rs.reward_id
	`, userID)
	h.Metrics.ObserveQuery("today", started, err)

	if err != nil {
		writeReadError(c, logger, "Failed to fetch today stocks", "internal server error", err)
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
			writeReadError(c, logger, "scan error", "internal server error", err)
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
		writeReadError(c, logger, "Error iterating today stocks rows", "internal server error", err)
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
