package stocky

import (
	"net/http"

	"github.com/LoganX64/stocky-api/internal/storage/models"
	"github.com/LoganX64/stocky-api/internal/utils"
	"github.com/LoganX64/stocky-api/internal/utils/response"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func PortfolioHandler(c *gin.Context) {
	userID, ok := parseUserID(c)
	if !ok {
		return
	}
	logger := logrus.WithFields(logrus.Fields{
		"request_id": requestID(c),
		"user_id":    userID,
	})

	rows, err := db.Query(`
		SELECT stock_symbol, adjusted_quantity, current_price, inr_value
		FROM user_portfolio
		WHERE user_id = $1
		  AND (stock_symbol NOT IN (
		      SELECT stock_symbol
		      FROM stock_events
		      WHERE event_type = 'delist' AND effective_date <= CURRENT_DATE
		  ))
	`, userID)

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

		item.Quantity = utils.RoundQuantity(item.Quantity)
		item.CurrentPrice = utils.RoundAmount(item.CurrentPrice)
		item.INRValue = utils.RoundAmount(item.INRValue)
		portfolio = append(portfolio, item)
	}
	response.WriteJson(c.Writer, http.StatusOK, map[string]interface{}{
		"userId":    userID,
		"portfolio": utils.OrEmpty(portfolio),
	})
}
