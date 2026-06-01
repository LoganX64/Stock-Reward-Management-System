package stocky

import (
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

	// Query from the view
	rows, err := h.DB.Query(`
		SELECT p.stock_symbol, p.adjusted_quantity, p.current_price, p.inr_value
		FROM user_portfolio p
		WHERE p.user_id = $1
		  AND NOT EXISTS (
		      SELECT 1
		      FROM stock_events se
		      WHERE se.event_type = 'delist'
		        AND se.effective_date <= CURRENT_DATE
		        AND UPPER(se.stock_symbol) = UPPER(p.stock_symbol)
		  )
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
		if err := rows.Scan(
			&item.StockSymbol,
			&item.Quantity,
			&item.CurrentPrice,
			&item.INRValue); err != nil {
			logger.WithError(err).Error("Failed to scan portfolio data for user ")
			response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
			return
		}

		// Apply rounding
		item.Quantity = utils.RoundQuantity(item.Quantity)
		item.CurrentPrice = utils.RoundAmount(item.CurrentPrice)
		item.INRValue = utils.RoundAmount(item.INRValue)
		portfolio = append(portfolio, item)
	}
	if err := rows.Err(); err != nil {
		logger.WithError(err).Error("Error iterating portfolio rows")
		response.WriteJson(c.Writer, http.StatusInternalServerError, response.ErrorResponse("An internal server error occurred"))
		return
	}
	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":    userID,
		"limit":     limit,
		"offset":    offset,
		"portfolio": utils.OrEmpty(portfolio),
	})
}
