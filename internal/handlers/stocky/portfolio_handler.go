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

func (h *Handler) PortfolioHandler(c *gin.Context) {
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

	// Every read in this response shares one read-only repeatable-read
	// snapshot. That keeps the page, the full-set totals, the unpriced set, and
	// the corporate-action outcome on the same valuation date and MVCC state.
	tx, err := h.beginReadSnapshot(c.Request.Context())
	if err != nil {
		logger.WithError(err).Error("Failed to start portfolio read snapshot")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}
	defer tx.Rollback()

	// Query from the shared outcome-aware view. Delist filtering and missing
	// price handling are applied inside user_portfolio so /portfolio and /stats
	// cannot diverge.
	rows, err := tx.Query(`
		SELECT
			p.stock_symbol,
			p.adjusted_quantity,
			p.current_price,
			p.inr_value,
			p.price_status,
			p.price_as_of,
			p.price_source,
			p.price_fetched_at
		FROM user_portfolio p
		WHERE p.user_id = $1
		ORDER BY p.stock_symbol
		LIMIT $2 OFFSET $3
	`, userID, limit, offset)

	if err != nil {
		logger.WithError(err).Error("Failed to fetch portfolio data for user ")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}

	portfolio := []models.PortfolioItem{}
	for rows.Next() {

		var item models.PortfolioItem
		var currentPrice decimal.NullDecimal
		var inrValue decimal.NullDecimal
		var priceAsOf sql.NullTime
		var priceSource sql.NullString
		var priceFetchedAt sql.NullTime
		if err := rows.Scan(
			&item.StockSymbol,
			&item.Quantity,
			&currentPrice,
			&inrValue,
			&item.PriceStatus,
			&priceAsOf,
			&priceSource,
			&priceFetchedAt); err != nil {
			rows.Close()
			logger.WithError(err).Error("Failed to scan portfolio data for user ")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
			return
		}

		// Apply rounding to available values and preserve NULL for missing
		// prices.
		item.Quantity = utils.RoundQuantity(item.Quantity)
		if currentPrice.Valid {
			item.CurrentPrice = decimal.NullDecimal{Decimal: utils.RoundAmount(currentPrice.Decimal), Valid: true}
		}
		if inrValue.Valid {
			item.INRValue = decimal.NullDecimal{Decimal: utils.RoundAmount(inrValue.Decimal), Valid: true}
		}
		if priceAsOf.Valid {
			t := priceAsOf.Time.UTC()
			item.PriceAsOf = &t
		}
		if priceSource.Valid {
			source := priceSource.String
			item.PriceSource = &source
		}
		if priceFetchedAt.Valid {
			t := priceFetchedAt.Time.UTC()
			item.PriceFetchedAt = &t
		}
		item.PriceFreshness = h.classifyPriceFreshness(item.PriceStatus, priceAsOf, priceSource)
		portfolio = append(portfolio, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		logger.WithError(err).Error("Error iterating portfolio rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}
	if err := rows.Close(); err != nil {
		logger.WithError(err).Error("Failed to close portfolio rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}

	unpricedSymbols, err := h.getUnpricedPortfolioSymbols(tx, userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch unpriced portfolio symbols")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}

	totalPortfolioValue, totalPositions, valuationDate, err := h.getPortfolioTotals(tx, userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch portfolio totals")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}

	freshness, err := h.getPortfolioFreshness(tx, userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch portfolio freshness")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}

	cashProceeds, writtenOffQuantity, cashInLieuProceeds, err := h.getUserCorporateActionCash(tx, userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch corporate action cash outcomes")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}

	if err := tx.Commit(); err != nil {
		logger.WithError(err).Error("Failed to commit portfolio read snapshot")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":              userID,
		"limit":               limit,
		"offset":              offset,
		"portfolio":           utils.OrEmpty(portfolio),
		"totalPortfolioValue": utils.RoundAmount(totalPortfolioValue),
		"totalPositions":      totalPositions,
		"valuationDate":       valuationDate,
		"valuationComplete":   len(unpricedSymbols) == 0,
		"unpricedPositions":   len(unpricedSymbols),
		"unpricedSymbols":     utils.OrEmpty(unpricedSymbols),
		"stalePositions":      freshness.StalePositions,
		"staleSymbols":        utils.OrEmpty(freshness.StaleSymbols),
		"pricesFresh":         freshness.PricesFresh,
		"cashProceeds":        utils.RoundAmount(cashProceeds),
		"cashInLieuProceeds":  utils.RoundAmount(cashInLieuProceeds),
		"writtenOffQuantity":  utils.RoundQuantity(writtenOffQuantity),
	})
}
