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

	// Query from the shared outcome-aware view. Delist filtering and missing
	// price handling are applied inside user_portfolio so /portfolio and /stats
	// cannot diverge.
	rows, err := h.DB.Query(`
		SELECT
			p.stock_symbol,
			p.adjusted_quantity,
			p.current_price,
			p.inr_value,
			p.price_status,
			p.price_as_of
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
	defer rows.Close()

	portfolio := []models.PortfolioItem{}
	for rows.Next() {

		var item models.PortfolioItem
		var currentPrice sql.NullFloat64
		var inrValue sql.NullFloat64
		var priceAsOf sql.NullTime
		if err := rows.Scan(
			&item.StockSymbol,
			&item.Quantity,
			&currentPrice,
			&inrValue,
			&item.PriceStatus,
			&priceAsOf); err != nil {
			logger.WithError(err).Error("Failed to scan portfolio data for user ")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
			return
		}

		// Apply rounding to available values and preserve NULL for missing
		// prices.
		item.Quantity = utils.RoundQuantity(item.Quantity)
		if currentPrice.Valid {
			v := utils.RoundAmount(currentPrice.Float64)
			item.CurrentPrice = &v
		}
		if inrValue.Valid {
			v := utils.RoundAmount(inrValue.Float64)
			item.INRValue = &v
		}
		if priceAsOf.Valid {
			t := priceAsOf.Time
			item.PriceAsOf = &t
		}
		portfolio = append(portfolio, item)
	}
	if err := rows.Err(); err != nil {
		logger.WithError(err).Error("Error iterating portfolio rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}

	unpricedSymbols, err := h.getUnpricedPortfolioSymbols(userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch unpriced portfolio symbols")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}

	cashProceeds, writtenOffQuantity, cashInLieuProceeds, err := h.getUserCorporateActionCash(userID)
	if err != nil {
		logger.WithError(err).Error("Failed to fetch corporate action cash outcomes")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}

	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":             userID,
		"limit":              limit,
		"offset":             offset,
		"portfolio":          utils.OrEmpty(portfolio),
		"valuationComplete":  len(unpricedSymbols) == 0,
		"unpricedPositions":  len(unpricedSymbols),
		"unpricedSymbols":    utils.OrEmpty(unpricedSymbols),
		"cashProceeds":       utils.RoundAmount(cashProceeds),
		"cashInLieuProceeds": utils.RoundAmount(cashInLieuProceeds),
		"writtenOffQuantity": utils.RoundQuantity(writtenOffQuantity),
	})
}
